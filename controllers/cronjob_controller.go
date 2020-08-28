/*
Copyright 2020 zou2699.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron"
	kbatch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ref "k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batch "github.com/zou2699/cronjob-tutorial/api/v1"
)

/*
	我们需要一个时钟，它可以让我们在测试中伪造时间。
*/

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
	Clock
}

/*
	我们将模拟时钟，以便在测试时更容易在时间上跳动，“真实”时钟仅调用`time.Now`。
*/
type realClock struct{}

func (_ realClock) Now() time.Time {
	return time.Now()
}

// clock 知道如何获取当前时间
// 它可以用来在测试时伪造时间。
type Clock interface {
	Now() time.Time
}

// +kubebuilder:docs-gen:collapse=Clock

/*
	注意，我们需要更多的RBAC权限-由于我们现在正在创建和管理job，因此我们需要这些权限，这意味着要添加更多[markers]（/ reference / markers / rbac.md）。
*/

// +kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=cronjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

/*
	现在，我们进入控制器的核心-协调器逻辑。*/
var (
	scheduledTimeAnnotation = "batch.tutorial.kubebuilder.io/scheduled-at"
)

/*
	1. 加载 CronJob

	2. 列出所有 active jobs，并更新状态

	3. 根据历史记录清理 old jobs

	4. 检查 Job 是否已被 suspended（如果被 suspended，请不要执行任何操作）

	5. 获取到下一次要 schedule 的 Job

	6. 运行新的 Job, 确定新 Job 没有超过 deadline 时间，且不会被我们 concurrency 规则 block

	7. 如果 Job 正在运行或者它应该下次运行，请重新排队
*/

func (r *CronJobReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("cronjob", req.NamespacedName)

	/*
		### 1: Load the CronJob by name

		我们使用 client 获取 CronJob。 所有的 client 方法都将 context（用来取消请求）作为其第一个参数， 并将所讨论的 object 作为其最后一个参数。
		Get 方法有点特殊， 因为它使用 NamespacedName 作为中间参数（大多数没有中间参数，如下所示）。

		最后，许多 client 方法也采用可变参数选项(也就是 “...”)。
	*/
	var cronJob batch.CronJob
	if err := r.Get(ctx, req.NamespacedName, &cronJob); err != nil {
		log.Error(err, "unable to fetch CronJob")
		//  我们将忽略未发现的错误，因为无法通过立即重新排队来解决它们（我们需要等待新的通知），并且我们可以在删除的请求上获取它们。
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	/*
		### 2: List all active jobs, and update the status

		要完全更新我们的状态，我们需要列出此命名空间中属于此CronJob的所有子作业。
		与Get类似，我们可以使用List方法列出子作业。
		注意，我们使用可变参数选项来设置名称空间和字段匹配（这实际上是我们在下面设置的索引查找）。
	*/

	var childJobs kbatch.JobList
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return ctrl.Result{}, err
	}
	/*
	      当得到所有的 Job 后，我们把 Job 的状态分为 active、successful和 failed, 并跟踪他们最近的运行情况，
	   	以便将其记录在 status 中。 请记住，status 应该可以从整体的状态重新构造， 因此从 root object 的状态读取信息通常不是一个好主意。
	   	相反，您应该在每次运行时重新构建它。 这就是我们在这里要做的。

	      我们可以使用 status conditions 来检查作业是“完成”、成功或失败。 我们将把这种逻辑放在匿名函数中，以使我们的代码更整洁。
	*/

	// 查找状态为 active 的 Jobs
	var activeJobs []*kbatch.Job
	var successfulJobs []*kbatch.Job
	var failedJobs []*kbatch.Job
	var mostRecentTime *time.Time // // 记录其最近一次运行时间以便更新状态

	/*
		如果一项工作的 “succeeded” 或 “failed” 的 Conditions 标记为 “true”，我们认为该工作 “finished”。
		Status.conditions 使我们可以向 objects 添加可扩展的状态信息， 其他人和 controller 可以通过检查这些状态信息以确定 Job 完成和健康状况。
	*/

	isJobFinished := func(job *kbatch.Job) (bool, kbatch.JobConditionType) {
		for _, c := range job.Status.Conditions {
			if (c.Type == kbatch.JobComplete || c.Type == kbatch.JobFailed) && c.Status == corev1.ConditionTrue {
				return true, c.Type
			}
		}
		return false, ""
	}
	// +kubebuilder:docs-gen:collapse=isJobFinished

	/*
		我们将使用匿名函数从创建 Job 时添加的 annotation 中获取到 Job 计划执行的时间。
	*/

	getScheduledTimeForJob := func(job *kbatch.Job) (*time.Time, error) {
		timeRaw := job.Annotations[scheduledTimeAnnotation]
		if len(timeRaw) == 0 {
			return nil, nil
		}

		timeParsed, err := time.Parse(time.RFC3339, timeRaw)
		if err != nil {
			return nil, err
		}
		return &timeParsed, nil
	}
	// +kubebuilder:docs-gen:collapse=getScheduledTimeForJob

	for i, job := range childJobs.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "": // ongoing
			activeJobs = append(activeJobs, &childJobs.Items[i])
		case kbatch.JobFailed:
			failedJobs = append(failedJobs, &childJobs.Items[i])
		case kbatch.JobComplete:
			successfulJobs = append(successfulJobs, &childJobs.Items[i])
		}
		// 将启动时间存放在注释中，当job生效时可以从中读取
		scheduledTimeForJob, err := getScheduledTimeForJob(&job)
		if err != nil {
			log.Error(err, "unable to parse schedule time for child job", "job", &job)
			continue
		}

		if scheduledTimeForJob != nil {
			if mostRecentTime == nil {
				mostRecentTime = scheduledTimeForJob
			} else if mostRecentTime.Before(*scheduledTimeForJob) { // 14<15, true
				mostRecentTime = scheduledTimeForJob
			}
		}
	}

	if mostRecentTime != nil {
		cronJob.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		cronJob.Status.LastScheduleTime = nil
	}
	cronJob.Status.Active = nil
	for _, activeJob := range activeJobs {
		jobRef, err := ref.GetReference(r.Scheme, activeJob)
		if err != nil {
			log.Error(err, "unable to make reference to active job", "job", activeJob)
			continue
		}
		cronJob.Status.Active = append(cronJob.Status.Active, *jobRef)
	}

	/*
		在这里，我们将以略高的日志记录级别记录观察到的作业数量，

		用于调试。 请注意，我们如何使用固定消息而不是格式字符串，

		并附加键值对以及更多信息。 这使得更容易

		过滤和查询日志行。
	*/
	log.V(1).Info("job count", "active jobs", len(activeJobs), "successful jobs", len(successfulJobs), "failed jobs", len(failedJobs))

	/*
		使用收集的日期，我们将更新CRD的状态。
		和以前一样，我们使用客户。 具体更新状态
		子资源，我们将使用客户端的“状态”部分以及“更新”
		方法。
		status子资源会忽略对规范的更改，因此冲突的可能性较小
		其他任何更新，并且可以具有单独的权限。
	*/
	if err := r.Status().Update(ctx, &cronJob); err != nil {
		log.Error(err, "unable to update CronJob status")
		return ctrl.Result{}, err
	}

	// 3 根据保留的历史版本数清理过久的job
	if cronJob.Spec.FailedJobsHistoryLimit != nil {
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i].Status.StartTime == nil {
				return failedJobs[j].Status.StartTime != nil
			}
			return failedJobs[i].Status.StartTime.Before(failedJobs[j].Status.StartTime)
		})

		for i, job := range failedJobs {
			if int32(i) >= int32(len(failedJobs))-*cronJob.Spec.FailedJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete old failed job", "job", job)
			} else {
				log.V(0).Info("deleted old failed job", "job", job)
			}
		}
	}

	if cronJob.Spec.SuccessfulJobsHistoryLimit != nil {
		sort.Slice(successfulJobs, func(i, j int) bool {
			if successfulJobs[i].Status.StartTime == nil {
				return successfulJobs[j].Status.StartTime != nil
			}
			return successfulJobs[i].Status.StartTime.Before(successfulJobs[j].Status.StartTime)
		})
		for i, job := range successfulJobs {
			if int32(i) >= int32(len(successfulJobs))-*cronJob.Spec.SuccessfulJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete old successful job", "job", job)
			} else {
				log.V(0).Info("deleted old successful job", "job", job)
			}
		}
	}

	// 4 检查是否被挂起
	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		log.V(1).Info("cronjob suspended,skipping")
		return ctrl.Result{}, nil
	}

	// 5 计算job下一次执行时间
	getNextSchedule := func(cronJob *batch.CronJob, now time.Time) (lastMissed time.Time, next time.Time, err error) {
		sched, err := cron.ParseStandard(cronJob.Spec.Schedule)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("Unparseable schedule %q: %v", cronJob.Spec.Schedule, err)
		}
		// 出于优化的目的，我们可以使用点技巧。从上一次观察到的执行时间开始执行，
		// 这个执行时间可以被在这里被读取。但是意义不大，因为我们刚更新了这个值。

		var earliestTime time.Time
		if cronJob.Status.LastScheduleTime != nil {
			earliestTime = cronJob.Status.LastScheduleTime.Time
		} else {
			earliestTime = cronJob.ObjectMeta.CreationTimestamp.Time
		}
		if cronJob.Spec.StartingDeadlineSeconds != nil {
			// 如果开始执行时间超过了截止时间，不再执行
			schedulingDeadline := now.Add(-time.Second * time.Duration(*cronJob.Spec.StartingDeadlineSeconds))
			if schedulingDeadline.After(earliestTime) {
				earliestTime = schedulingDeadline
			}
		}
		if earliestTime.After(now) {
			return time.Time{}, sched.Next(now), nil
		}
		starts := 0
		for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
			lastMissed = t
			// 一个 CronJob 可能会遗漏多次执行。举个例子，周五5:00pm技术人员下班后，
			// 控制器在5:01pm发生了异常。然后直到周二早上才有技术人员发现问题并
			// 重启控制器。那么所有的以1小时为周期执行的定时任务，在没有技术人员
			// 进一步的干预下，都会有80多个 job 在恢复正常后一并启动（如果 job 允许
			// 多并发和延迟启动）

			// 如果 CronJob 的某些地方出现异常，控制器或 apiservers (用于设置任务创建时间)
			// 的时钟不正确, 那么就有可能出现错过很多次执行时间的情形（跨度可达数十年）
			// 这将会占满控制器的CPU和内存资源。这种情况下，我们不需要列出错过的全部
			// 执行时间。
			starts++
			if starts > 100 {
				// 获取不到最近一次执行时间，直接返回空切片
				return time.Time{}, time.Time{}, fmt.Errorf("too many missed start times (> 100). Set or decrease .spec.startingDeadlineSeconds or check clock skew")
			}
		}
		return lastMissed, sched.Next(now), nil
	}
	// +kubebuilder:docs-gen:collapse=getNextSchedule

	// 计算出定时任务下一次执行时间（或是遗漏的执行时间）
	missedRun, nextRun, err := getNextSchedule(&cronJob, r.Now())
	if err != nil {
		log.Error(err, "unable to figure out CronJob schedule")
		// 重新排队直到有更新修复这次定时任务调度，不必返回错误
		return ctrl.Result{}, nil
	}

	scheduledResult := ctrl.Result{RequeueAfter: nextRun.Sub(r.Now())} // 保存以便别处复用
	log.WithValues("now", r.Now(), "next run", nextRun)

	// 6 如果job符合执行时机，并且没有超出截止时间，且不被并发策略阻塞，执行该job
	// 	如果 job 遗漏了一次执行，且还没超出截止时间，把遗漏的这次执行也不上
	if missedRun.IsZero() {
		log.V(1).Info("no upcoming scheduled times, sleeping until next")
		return scheduledResult, nil
	}

	// 确保错过的执行没有超过截止时间
	log.WithValues("current run", missedRun)
	tooLate := false
	if cronJob.Spec.StartingDeadlineSeconds != nil {
		tooLate = missedRun.Add(time.Duration(*cronJob.Spec.StartingDeadlineSeconds) * time.Second).Before(r.Now())
	}
	if tooLate {
		log.V(1).Info("missed staring deadline for last run, sleeping till next")
		return scheduledResult, nil
	}

	/*
		如果确认 job 需要实际执行。我们有三种策略执行该 job。
		要么先等待现有的 job 执行完后，在启动本次 job；
		或是直接覆盖取代现有的job；
		或是不考虑现有的 job，直接作为新的 job 执行。
		因为缓存导致的信息有所延迟， 当更新信息后需要重新排队。
	*/

	// 确定要 job 的执行策略 —— 并发策略可能禁止多个job同时运行
	if cronJob.Spec.ConcurrencyPolicy == batch.ForbidConcurrent && len(activeJobs) > 0 {
		log.V(1).Info("concurrency policy blocks concurrent runs, skipping", "num active", len(activeJobs))
		return scheduledResult, nil
	}

	// 直接覆盖现有 job
	if cronJob.Spec.ConcurrencyPolicy == batch.ReplaceConcurrent {
		for _, activeJob := range activeJobs {
			// we don't care if the job was already deleted
			if err := r.Delete(ctx, activeJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete active job", "job", activeJob)
				return ctrl.Result{}, err
			}
		}
	}

	/*
		基于 CronJob 模版构建 job，从模板复制 spec 及对象的元信息。
		然后在注解中设置执行时间，这样我们可以在每次的调谐中获取起作为“上一次执行时间”
		最后，还需要设置 owner reference字段。当我们删除 CronJob 时，Kubernetes 垃圾收集 器会根据这个字段对应的 job。同时，当某个job状态发生变更（创建，删除，完成）时， controller-runtime 可以根据这个字段识别出要对那个 CronJob 进行调谐。
	*/
	constructJobForCronJob := func(cronJob *batch.CronJob, scheduledTime time.Time) (*kbatch.Job, error) {
		// job 名称带上执行时间以确保唯一性，避免排定执行时间的 job 创建两次
		name := fmt.Sprintf("%s-%d", cronJob.Name, scheduledTime.Unix())
		job := &kbatch.Job{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      make(map[string]string),
				Annotations: make(map[string]string),
				Name:        name,
				Namespace:   cronJob.Namespace,
			},
			Spec: *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
		}
		for k, v := range cronJob.Spec.JobTemplate.Annotations {
			job.Annotations[k] = v
		}
		job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)
		for k, v := range cronJob.Spec.JobTemplate.Labels {
			job.Labels[k] = v
		}
		if err := ctrl.SetControllerReference(cronJob, job, r.Scheme); err != nil {
			return nil, err
		}
		return job, nil
	}

	// 构建 job
	job, err := constructJobForCronJob(&cronJob, missedRun)
	if err != nil {
		log.Error(err, "unable to construct job from template")
		// job 的spec没有变更，无需重新排队
		return scheduledResult, nil
	}

	// ...在集群中创建 job

	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "unbale to create Job for CronJob", "job", job)
		return ctrl.Result{}, err
	}
	log.V(1).Info("created for CronJob run", "job", job)

	// 当有 job 进入运行状态后，重新排队，同时更新状态
	return scheduledResult, nil

}

/*
最后，我们还要完善下我们的启动过程。为了让调谐器可以通过 job 的 owner 值快速找到 job。
我们需要一个索引。声明一个索引键，后续我们可以将其用于 client 的虚拟变量名中，
从 job 对象中提取索引值。此处的索引会帮我们处理好 namespaces 的映射关系。
所以如果 job 有 owner 值，我们快速地获取 owner 值。

另外，我们需要告知 manager，这个控制器拥有哪些 job。当对应的 job 发生变更或被删除时， 自动调用调谐器对 CronJob 进行调谐。
*/

var (
	jobOwnerKey = ".metadata.controller"
	apiGVStr    = batch.GroupVersion.String()
)

func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// 此处不是测试，我们需要创建一个真实的时钟
	if r.Clock == nil {
		r.Clock = realClock{}
	}

	if err := mgr.GetFieldIndexer().IndexField(&kbatch.Job{}, jobOwnerKey, func(rawObj runtime.Object) []string {
		// 获取 job 对象，提取 owner...
		job := rawObj.(*kbatch.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		// ...确保 owner 是个 CronJob...
		if owner.APIVersion != apiGVStr || owner.Kind != "CronJob" {
			return nil
		}
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&batch.CronJob{}).
		Owns(&kbatch.Job{}).
		Complete(r)
}
