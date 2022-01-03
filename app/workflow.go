package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	recipe "github.com/coreos/etcd/contrib/recipes"
	"github.com/gorhill/cronexpr"
	"github.com/holdno/gopherCron/common"
	"github.com/holdno/gopherCron/errors"
	"github.com/holdno/gopherCron/pkg/warning"
	"github.com/holdno/gopherCron/protocol"
	"github.com/holdno/gopherCron/utils"
	"github.com/holdno/rego"

	"github.com/holdno/gocommons/selection"
	"github.com/jinzhu/gorm"
)

func (a *app) CreateWorkflow(userID int64, data common.Workflow) error {

	var (
		tx  = a.store.BeginTx()
		err error
	)
	defer func() {
		if r := recover(); r != nil && err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()
	if err = a.store.Workflow().Create(tx, &data); err != nil {
		return errors.NewError(errors.CodeInternalError, "创建workflow失败").WithLog(err.Error())
	}

	if err = a.store.UserWorkflowRelevance().Create(tx, &common.UserWorkflowRelevance{
		UserID:     userID,
		WorkflowID: data.ID,
		CreateTime: time.Now().Unix(),
	}); err != nil {
		return errors.NewError(http.StatusInternalServerError, "创建workflow用户关联关系失败").WithLog(err.Error())
	}
	return nil
}

func checkUserWorkflowPermission(checkFunc interface {
	GetUserWorkflowRelevance(userID int64, workflowID int64) (*common.UserWorkflowRelevance, error)
}, userID, workflowID int64) error {
	if userID == 1 {
		return nil
	}
	exist, err := checkFunc.GetUserWorkflowRelevance(userID, workflowID)
	if err != nil && err != gorm.ErrRecordNotFound {
		return errors.NewError(http.StatusInternalServerError, "检测用户权限失败").WithLog(err.Error())
	}
	if exist == nil {
		return errors.NewError(http.StatusUnauthorized, "无权编辑该workflow")
	}
	return nil
}

type CreateWorkflowTaskArgs struct {
	WorkflowTaskInfo
	Dependencies []WorkflowTaskInfo
}

func (a *app) CreateWorkflowTask(userID, workflowID int64, taskList []CreateWorkflowTaskArgs) error {
	err := checkUserWorkflowPermission(a.store.UserWorkflowRelevance(), userID, workflowID)
	if err != nil {
		return err
	}
	workflowTaskList, err := a.store.WorkflowTask().GetList(workflowID)
	if err != nil && err != gorm.ErrRecordNotFound {
		return errors.NewError(errors.CodeInternalError, "创建workflow 任务信息失败").WithLog(err.Error())
	}

	var needToDelete []int64
	for _, v := range workflowTaskList {
		needToDelete = append(needToDelete, v.ID)
	}
	var needToCreate []common.WorkflowTask
	for _, v := range taskList {
		if len(v.Dependencies) > 0 {
			for _, vv := range v.Dependencies {
				needToCreate = append(needToCreate, common.WorkflowTask{
					WorkflowID:          workflowID,
					TaskID:              v.TaskID,
					ProjectID:           v.ProjectID,
					DependencyTaskID:    vv.TaskID,
					DependencyProjectID: vv.ProjectID,
					CreateTime:          time.Now().Unix(),
				})
			}
		} else {
			needToCreate = append(needToCreate, common.WorkflowTask{
				WorkflowID:          workflowID,
				TaskID:              v.TaskID,
				ProjectID:           v.ProjectID,
				DependencyTaskID:    "",
				DependencyProjectID: 0,
				CreateTime:          time.Now().Unix(),
			})
		}
	}

	tx := a.store.BeginTx()
	defer func() {
		if r := recover(); r != nil || err != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()
	if err = a.store.WorkflowTask().DeleteList(tx, needToDelete); err != nil {
		return errors.NewError(errors.CodeInternalError, "创建workflow 任务信息失败, 解除任务关联失败").WithLog(err.Error())
	}

	for _, v := range needToCreate {
		if err = a.store.WorkflowTask().Create(tx, &v); err != nil {
			return errors.NewError(errors.CodeInternalError, "创建workflow 任务信息失败, 创建任务关联关系失败").WithLog(err.Error())
		}
	}
	return nil
}

func disposeWorkflowTaskData(workflowTaskList []common.WorkflowTask, task WorkflowTaskInfo, dependencies []WorkflowTaskInfo) ([]int64, []common.WorkflowTask) {
	dependMap := make(map[WorkflowTaskInfo]bool)
	for _, v := range dependencies {
		dependMap[v] = true
	}

	var needToDelete []int64
	var workflowID int64
	for _, v := range workflowTaskList {
		workflowID = v.WorkflowID
		key := WorkflowTaskInfo{
			TaskID:    v.DependencyTaskID,
			ProjectID: v.DependencyProjectID,
		}
		if dependMap[key] {
			// 删除已经存在的key
			delete(dependMap, key)
			continue
		}
		needToDelete = append(needToDelete, v.ID)
	}

	var needToCreate []common.WorkflowTask
	for k := range dependMap {
		needToCreate = append(needToCreate, common.WorkflowTask{
			WorkflowID:          workflowID,
			TaskID:              task.TaskID,
			ProjectID:           task.ProjectID,
			DependencyTaskID:    k.TaskID,
			DependencyProjectID: k.ProjectID,
			CreateTime:          time.Now().Unix(),
		})
	}

	if len(needToCreate) == 0 && len(workflowTaskList) == 0 {
		needToCreate = append(needToCreate, common.WorkflowTask{
			WorkflowID:          workflowID,
			TaskID:              task.TaskID,
			ProjectID:           task.ProjectID,
			DependencyTaskID:    "",
			DependencyProjectID: 0,
			CreateTime:          time.Now().Unix(),
		})
	}

	return needToDelete, needToCreate
}

func (a *app) GetWorkflowList(opts common.GetWorkflowListOptions, page, pagesize uint64) ([]common.Workflow, int, error) {
	// TODO get user workflow
	selector := selection.NewSelector()
	if len(opts.IDs) > 0 {
		selector.AddQuery(selection.NewRequirement("id", selection.In, opts.IDs))
	}
	if opts.Title != "" {
		selector.AddQuery(selection.NewRequirement("title", selection.Like, opts.Title))
	}
	list, err := a.store.Workflow().GetList(selector, page, pagesize)
	if err != nil {
		return nil, 0, errors.NewError(http.StatusInternalServerError, "获取workflow列表失败").WithLog(err.Error())
	}

	total, err := a.store.Workflow().GetTotal(selector)
	if err != nil {
		return nil, 0, errors.NewError(http.StatusInternalServerError, "获取workflow总记录数失败").WithLog(err.Error())
	}

	return list, total, nil
}

func (a *app) GetWorkflowTasks(workflowID int64) ([]common.WorkflowTask, error) {
	list, err := a.store.WorkflowTask().GetList(workflowID)
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, errors.NewError(http.StatusInternalServerError, "获取workflow任务列表失败").WithLog(err.Error())
	}

	return list, nil
}

func (a *app) GetUserWorkflows(userID int64) ([]int64, error) {
	list, err := a.store.UserWorkflowRelevance().GetUserWorkflows(userID)
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, errors.NewError(http.StatusInternalServerError, "获取角色下关联的workflow失败").WithLog(err.Error())
	}
	var result []int64
	for _, v := range list {
		result = append(result, v.WorkflowID)
	}
	return result, nil
}

func (a *app) UpdateWorkflow(userID int64, data common.Workflow) error {
	err := checkUserWorkflowPermission(a.store.UserWorkflowRelevance(), userID, data.ID)
	if err != nil {
		return err
	}

	if err = a.store.Workflow().Update(nil, data); err != nil {
		return errors.NewError(http.StatusInternalServerError, "更新workflow失败").WithLog(err.Error())
	}
	return nil
}

func (a *app) DeleteWorkflow(userID int64, workflowID int64) error {
	err := checkUserWorkflowPermission(a.store.UserWorkflowRelevance(), userID, workflowID)
	if err != nil {
		return err
	}

	if err = a.store.Workflow().Delete(nil, workflowID); err != nil {
		return errors.NewError(http.StatusInternalServerError, "删除workflow失败").WithLog(err.Error())
	}
	return nil
}

type workflowRunner struct {
	etcd              *clientv3.Client
	app               App
	plans             sync.Map
	planCounter       int64
	nextWorkflow      common.Workflow
	scheduleEventChan chan *common.TaskEvent
	taskResultChan    chan string

	queue *recipe.Queue

	ctx        context.Context
	cancelFunc context.CancelFunc
	isClose    bool
}

func NewWorkflowRunner(app App, cli *clientv3.Client) (*workflowRunner, error) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &workflowRunner{
		app:               app,
		etcd:              app.GetEtcdClient(),
		ctx:               ctx,
		cancelFunc:        cancel,
		queue:             recipe.NewQueue(cli, common.BuildWorkflowQueuePrefixKey()),
		taskResultChan:    make(chan string, 10),
		scheduleEventChan: make(chan *common.TaskEvent, 10),
	}

	list, _, err := app.GetWorkflowList(common.GetWorkflowListOptions{}, 1, 1000)
	if err != nil {
		return nil, err
	}

	for _, v := range list {
		runner.SetPlan(v)
	}

	app.Go(func() {
		for {
			result, err := runner.queue.Dequeue()
			if err != nil {
				return
			}
			if runner.isClose {
				runner.queue.Enqueue(result)
				return
			}
			runner.taskResultChan <- result
		}
	})

	return runner, nil
}

func (r *workflowRunner) Close() {
	if r.isClose {
		return
	}
	r.isClose = true
	r.cancelFunc()
}

type WorkflowPlan struct {
	runner    *workflowRunner
	Workflow  common.Workflow
	Expr      *cronexpr.Expression // 解析后的cron表达式
	NextTime  time.Time
	Tasks     map[WorkflowTaskInfo]*common.TaskInfo
	TaskFlow  map[WorkflowTaskInfo][]WorkflowTaskInfo // map[任务][]依赖
	planState *PlanState
}

func (p *WorkflowPlan) Finished(scheduleError error) error {
	p.planState.Status = common.TASK_STATUS_DONE_V2
	if scheduleError != nil {
		p.planState.Status = common.TASK_STATUS_FAIL_V2
	}

	states, err := getWorkflowAllTaskStates(p.runner.etcd.KV, p.Workflow.ID)
	if err != nil {
		return err
	}

	failedReason := strings.Builder{}
	for _, v := range states {
		if v.CurrentStatus == common.TASK_STATUS_FAIL_V2 {
			p.planState.Status = common.TASK_STATUS_FAIL_V2
			// get task info
			taskDetail, err := getTaskDetail(p.runner.etcd.KV, v.ProjectID, v.TaskID)
			if err != nil {
				// log
				return err
			}
			failedReason.WriteString(taskDetail.Name)
			failedReason.WriteString(" 任务执行失败\n")
		}
	}
	if scheduleError != nil {
		failedReason.WriteString(scheduleError.Error() + "\n")
	}

	p.planState.Reason = failedReason.String()
	p.planState.Records = states

	result, err := json.Marshal(p.planState)
	if err != nil {
		return err
	}

	// workflow执行结果写入数据库
	fmt.Println("workflow result:", string(result))

	if err = clearWorkflowKeys(p.runner.etcd.KV, p.Workflow.ID); err != nil {
		return err
	}
	return nil
}

type taskFlowItem struct {
	Task WorkflowTaskInfo
	Deps []WorkflowTaskInfo
}

func (a *workflowRunner) TryStartPlan(plan *WorkflowPlan) error {
	// 获取当前plan是否在运行中
	// TODO lock
	running, err := plan.IsRunning()
	if err != nil || running {
		// TODO latest workflow not compalete
		return err
	}

	if err = plan.SetRunning(); err != nil {
		return err
	}

	fmt.Println("can schedule")
	needToScheduleTasks, finished, err := plan.CanSchedule()
	if err != nil && err != ErrWorkflowFailed {
		return err
	}

	if finished {
		plan.Finished(err)
		return nil
	}

	fmt.Println("need to schedule", needToScheduleTasks)
	for _, v := range needToScheduleTasks {
		a.scheduleEventChan <- common.BuildTaskEvent(common.TASK_EVENT_WORKFLOW_SCHEDULE, plan.Tasks[v])
		fmt.Println("send schedule event")
	}
	return nil
}

var (
	ErrWorkflowFailed = errors.NewError(http.StatusInternalServerError, "workflow任务失败")
)

// 判断下一步可调度的任务
func (s *WorkflowPlan) CanSchedule() ([]WorkflowTaskInfo, bool, error) {
	var (
		readys        []WorkflowTaskInfo
		taskStatesMap      = make(map[WorkflowTaskInfo]*WorkflowTaskStates)
		finished      bool = true
	)

	states, err := getWorkflowTasksStates(s.runner.etcd.KV, common.BuildWorkflowTaskStatusKeyPrefix(s.Workflow.ID))
	if err != nil {
		return nil, false, err
	}

	for _, v := range states {
		taskStatesMap[WorkflowTaskInfo{v.ProjectID, v.TaskID}] = v
	}

	for task, deps := range s.TaskFlow {
		taskStates, exist := taskStatesMap[WorkflowTaskInfo{task.ProjectID, task.TaskID}]
		if exist && taskStates.CurrentStatus == common.TASK_STATUS_DONE_V2 {
			continue
		}

		// 检查依赖的任务是否都已结束
		ok := true
		for _, check := range deps {
			if check.TaskID != "" {
				states := taskStatesMap[check]
				if states == nil || states.CurrentStatus != common.TASK_STATUS_DONE_V2 {
					ok = false
					break
				}
			}
		}
		if !ok { // 上游还未跑完
			finished = false
			continue
		}

		if taskStates == nil {
			taskStates = &WorkflowTaskStates{
				CurrentStatus: common.TASK_STATUS_NOT_RUNNING_V2,
			}
		}

		switch taskStates.CurrentStatus {
		case common.TASK_STATUS_RUNNING_V2:
			finished = false
			fallthrough
		case common.TASK_STATUS_FAIL_V2:
			// 判断是否已经重复跑3次
			if taskStates.ScheduleCount >= common.WORKFLOW_SCHEDULE_LIMIT {
				return nil, true, ErrWorkflowFailed
			}
			fallthrough
		case common.TASK_STATUS_NOT_RUNNING_V2:
			finished = false
			readys = append(readys, task)
		case common.TASK_STATUS_STARTING_V2: // 异常补救
			if taskStates.ScheduleCount >= common.WORKFLOW_SCHEDULE_LIMIT {
				return nil, true, ErrWorkflowFailed
			}
			finished = false
			readys = append(readys, task)
			if time.Now().Unix()-taskStates.StartTime > 5 {
				taskStates.CurrentStatus = common.TASK_STATUS_NOT_RUNNING_V2
				latestRecord := taskStates.GetLatestScheduleRecord()
				taskStates.ScheduleRecords = append(taskStates.ScheduleRecords, &WorkflowTaskScheduleRecord{
					TmpID:     latestRecord.TmpID,
					Status:    common.TASK_STATUS_NOT_RUNNING_V2,
					EventTime: time.Now().Unix(),
				})
				newStates, _ := json.Marshal(taskStates)
				ctx, _ := utils.GetContextWithTimeout()
				if _, err = s.runner.etcd.KV.Put(ctx, common.BuildWorkflowTaskStatusKey(taskStates.WorkflowID, taskStates.ProjectID, taskStates.TaskID), string(newStates)); err != nil {
					return nil, false, err
				}
			}
		default:
		}
	}

	if finished {
		return nil, finished, nil
	}

	return readys, finished, nil
}

type WorkflowTaskInfo struct {
	ProjectID int64  `json:"project_id"`
	TaskID    string `json:"task_id"`
}

func inverseGraph(graph map[WorkflowTaskInfo][]WorkflowTaskInfo) (igraph map[WorkflowTaskInfo][]WorkflowTaskInfo) {
	igraph = make(map[WorkflowTaskInfo][]WorkflowTaskInfo)
	for node, outcomes := range graph {
		for _, outcome := range outcomes {
			igraph[outcome] = append(igraph[outcome], node)
		}
		if _, existed := igraph[node]; !existed {
			igraph[node] = make([]WorkflowTaskInfo, 0)
		}
	}
	return igraph
}

// igraph map[任务][]依赖
// return 执行顺序
func kahn(igraph map[WorkflowTaskInfo][]WorkflowTaskInfo) (L []WorkflowTaskInfo, err error) {
	var S []WorkflowTaskInfo
	graph := inverseGraph(igraph)
	inCountMap := make(map[WorkflowTaskInfo]int)
	for node, incomes := range igraph {
		inCountMap[node] = len(incomes)
		if inCountMap[node] == 0 {
			S = append(S, node)
		}
	}

	for {
		if len(S) == 0 {
			break
		}
		node := S[0]
		S = S[1:]
		L = append(L, node)

		for _, outcome := range graph[node] {
			inCountMap[outcome]--
			if inCountMap[outcome] == 0 {
				S = append(S, outcome)
			}
		}
	}

	for node, inCount := range inCountMap {
		if inCount != 0 {
			err = fmt.Errorf("Invalid DAG node %v", node)
			return
		}
	}

	return

}

func (a *workflowRunner) GetPlan(id int64) *WorkflowPlan {
	data, exist := a.plans.Load(id)
	if !exist {
		return nil
	}
	return data.(*WorkflowPlan)
}

func (a *workflowRunner) SetPlan(data common.Workflow) error {
	atomic.AddInt64(&a.planCounter, 1)

	tasks, err := a.app.GetWorkflowTasks(data.ID)
	if err != nil {
		return err
	}

	fmt.Println("flow tasks", tasks)

	plan := &WorkflowPlan{
		runner:   a,
		Workflow: data,
		Tasks:    make(map[WorkflowTaskInfo]*common.TaskInfo),
		TaskFlow: make(map[WorkflowTaskInfo][]WorkflowTaskInfo),
	}

	state, err := getWorkflowPlanState(a.etcd.KV, data.ID)
	if err != nil {
		return err
	}
	plan.planState = state // maybe nil

	depsMap := make(map[WorkflowTaskInfo][]WorkflowTaskInfo)
	for _, v := range tasks {
		key := WorkflowTaskInfo{
			TaskID:    v.TaskID,
			ProjectID: v.ProjectID,
		}
		depsMap[key] = append(depsMap[key], WorkflowTaskInfo{
			TaskID:    v.DependencyTaskID,
			ProjectID: v.DependencyProjectID,
		})

		if _, exist := plan.Tasks[key]; !exist {
			plan.Tasks[key], err = a.app.GetTask(key.ProjectID, key.TaskID)
			if err != nil {
				return err
			}
			plan.Tasks[key].FlowInfo = &common.WorkflowInfo{
				WorkflowID: plan.Workflow.ID,
			}
		}
	}

	plan.TaskFlow = depsMap
	expr, err := cronexpr.Parse(data.Cron)
	if err != nil {
		return err
	}

	plan.Expr = expr
	plan.NextTime = expr.Next(time.Now())
	a.plans.Store(data.ID, plan)
	return nil
}

func (a *workflowRunner) DelPlan(id int64) {
	atomic.AddInt64(&a.planCounter, -1)
	a.plans.Delete(id)
}

func (a *workflowRunner) PlanCount() int64 {
	return atomic.LoadInt64(&a.planCounter)
}

func (a *workflowRunner) PlanRange(f func(key int64, value *WorkflowPlan) bool) {
	a.plans.Range(func(key, value interface{}) bool {
		f(key.(int64), value.(*WorkflowPlan))
		return true
	})
}

func (a *workflowRunner) TrySchedule() time.Duration {
	var (
		now      time.Time
		nearTime *time.Time
	)

	// 如果当前任务调度表中没有任务的话 可以随机睡眠后再尝试
	if a.PlanCount() == 0 {
		return time.Second
	}

	now = time.Now()
	// 遍历所有任务
	a.PlanRange(func(workflowID int64, plan *WorkflowPlan) bool {
		// 如果调度时间是在现在或之前再或者为临时调度任务
		if plan.NextTime.Before(now) || plan.NextTime.Equal(now) {
			// 尝试执行任务
			// 因为可能上一次任务还没执行结束
			if err := a.TryStartPlan(plan); err != nil {
				fmt.Println("执行workflow失败", err.Error())
			}
			plan.NextTime = plan.Expr.Next(now) // 更新下一次执行时间
		}

		// 获取下一个要执行任务的时间
		if nearTime == nil || plan.NextTime.Before(*nearTime) {
			nearTime = &plan.NextTime
		}

		return true
	})

	// 下次调度时间 (最近要执行的任务调度时间 - 当前时间)
	return (*nearTime).Sub(now)
}

func (a *workflowRunner) Loop() {
	var (
		taskEvent     *common.TaskEvent
		scheduleAfter time.Duration
		scheduleTimer *time.Timer
		executeResult string
	)

	scheduleAfter = a.TrySchedule()

	// 调度定时器
	scheduleTimer = time.NewTimer(scheduleAfter)

	fmt.Printf("start workflow, next schedule after %d second\n", scheduleAfter/time.Second)

	for {
		select {
		case taskEvent = <-a.scheduleEventChan:
			// 对内存中的任务进行增删改查
			a.handleTaskEvent(taskEvent)
		case executeResult = <-a.taskResultChan:
			var execResult protocol.TaskFinishedQueueContent
			_ = json.Unmarshal([]byte(executeResult), &execResult)
			switch execResult.Version {
			case protocol.QueueItemV1:

				var result protocol.TaskFinishedQueueItemV1
				_ = json.Unmarshal(execResult.Data, &result)
				if err := a.handleTaskResultV1(result); err != nil {
					if err = a.queue.Enqueue(executeResult); err != nil {
						a.app.Warning(warning.WarningData{
							Type:      warning.WarningTypeSystem,
							Data:      fmt.Sprintf("任务结果消费出错，重新入队失败, %s", err.Error()),
							TaskName:  result.TaskID,
							ProjectID: result.ProjectID,
						})
					}
				}

			}
		case <-scheduleTimer.C: // 最近的一个调度任务到期执行
		}

		if a.isClose {
			scheduleTimer.Stop()
			continue
		}
		// 每次触发事件后 重新计算下次调度任务时间
		scheduleAfter = a.TrySchedule()
		scheduleTimer.Reset(scheduleAfter)
	}
}

func (a *workflowRunner) handleTaskResultV1(data protocol.TaskFinishedQueueItemV1) error {
	next := true
	err := rego.Retry(func() error {
		_, err := concurrency.NewSTM(a.etcd, func(s concurrency.STM) error {
			planFinished, err := setWorkFlowTaskFinished(s, data)
			if err != nil {
				return err
			}

			// 任务如果失败三次，则终止整个workflow
			if planFinished {
				next = false
				plan := a.GetPlan(data.WorkflowID)
				if plan != nil {
					plan.Finished(nil)
				}
			}
			return nil
		})
		return err
	})
	if err != nil {
		return err
	}

	if !next {
		return nil
	}
	plan := a.GetPlan(data.WorkflowID)
	if plan == nil {
		return nil
	}

	err = rego.Retry(func() error {
		return a.TryStartPlan(plan)
	})
	if err != nil {
		return err.Latest()
	}
	return nil
}

func (a *workflowRunner) handleTaskEvent(event *common.TaskEvent) {
	fmt.Println("get task event", *event)
	switch event.EventType {
	case common.TASK_EVENT_WORKFLOW_SCHEDULE:
		err := rego.Retry(func() error {
			if err := scheduleTask(a.etcd, event.Task); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			a.app.Warning(warning.WarningData{
				Data: fmt.Sprintf("workflow任务调度失败，workflow_id: %d\n%s",
					event.Task.FlowInfo.WorkflowID, err.Error()),
				Type:      warning.WarningTypeSystem,
				TaskName:  event.Task.Name,
				ProjectID: event.Task.ProjectID,
			})
		}
	}
}

// TODO
func (p *WorkflowPlan) IsRunning() (bool, error) {
	if p.planState == nil {
		states, err := getWorkflowPlanState(p.runner.etcd.KV, p.Workflow.ID)
		if err != nil {
			return false, err
		}
		if states == nil {
			return false, nil
		}
		p.planState = states
	}
	now := time.Now()

	if now.Unix()-p.planState.LatestTryTime > p.Expr.Next(now).Unix()-now.Unix() {
		return false, nil
	}
	return p.planState.Status == common.TASK_STATUS_RUNNING_V2, nil
}

func (p *WorkflowPlan) SetRunning() error {
	newState, err := setWorkflowPlanRunning(p.runner.etcd, p.Workflow.ID)
	if err != nil {
		return err
	}
	p.planState = newState
	return nil
}
