package scheduler

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/cloud"
	"github.com/evergreen-ci/evergreen/db"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/model/distro"
	"github.com/evergreen-ci/evergreen/model/event"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/model/task"
	"github.com/evergreen-ci/evergreen/model/version"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/mongodb/anser/bsonutil"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/pkg/errors"
)

// Responsible for prioritizing and scheduling tasks to be run, on a per-distro
// basis.
type Scheduler struct {
	*evergreen.Settings
	TaskPrioritizer
	TaskQueuePersister
	HostAllocator

	GetExpectedDurations TaskDurationEstimator
	FindRunnableTasks    TaskFinder
}

const (
	underwaterPruningEnabled = true
	allDistros               = ""
)

// versionBuildVariant is used to keep track of the version/buildvariant fields
// for tasks that are to be split by distro
type versionBuildVariant struct {
	Version, BuildVariant string
}

// Schedule all of the tasks to be run.  Works by finding all of the tasks that
// are ready to be run, splitting them by distro, prioritizing them, and saving
// the per-distro queues.  Then determines the number of new hosts to spin up
// for each distro, and spins them up.
func (s *Scheduler) Schedule(ctx context.Context) error {
	startAt := time.Now()

	if err := model.UpdateStaticHosts(); err != nil {
		return errors.Wrap(err, "error updating static hosts")
	}

	if err := underwaterUnschedule(allDistros); err != nil {
		return errors.Wrap(err, "problem unscheduled underwater tasks")
	}

	runnableTasks, err := s.FindRunnableTasks(allDistros)
	if err != nil {
		return errors.Wrap(err, "Error finding runnable tasks")
	}

	grip.Info(message.Fields{
		"message":  "found runnable tasks",
		"runner":   RunnerName,
		"count":    len(runnableTasks),
		"duration": time.Since(startAt),
		"span":     time.Since(startAt).String(),
	})

	// split the tasks by distro
	tasksByDistro, taskRunDistros, err := s.splitTasksByDistro(runnableTasks)
	if err != nil {
		return errors.Wrap(err, "Error splitting tasks by distro to run on")
	}

	// load in all of the distros
	distros, err := distro.Find(distro.All)
	if err != nil {
		return errors.Wrap(err, "Error finding distros")
	}

	// get the expected run duration of all runnable tasks
	taskExpectedDuration, err := s.GetExpectedDurations(runnableTasks)

	if err != nil {
		return errors.Wrap(err, "Error getting expected task durations")
	}

	distroInputChan := make(chan distroSchedulerInput, len(distros))

	// put all of the needed input for the distro scheduler into a channel to be read by the
	// distro scheduling loop.
	for _, d := range distros {
		runnableTasksForDistro := tasksByDistro[d.Id]
		if len(runnableTasksForDistro) == 0 {
			continue
		}
		distroInputChan <- distroSchedulerInput{
			distroId:               d.Id,
			runnableTasksForDistro: runnableTasksForDistro,
		}

	}

	if ctx.Err() != nil {
		return errors.New("scheduling run canceled.")
	}

	// close the channel to signal that the loop reading from it can terminate
	close(distroInputChan)
	workers := runtime.NumCPU()

	wg := sync.WaitGroup{}
	wg.Add(workers)

	// make a channel to collect all of function results from scheduling the distros
	distroSchedulerResultChan := make(chan distroSchedulerResult)

	ds := &distroSchedueler{
		TaskPrioritizer:    s.TaskPrioritizer,
		TaskQueuePersister: s.TaskQueuePersister,
	}

	// for each worker, create a new goroutine
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			// read the inputs for scheduling this distro
			for d := range distroInputChan {
				distroStartTime := time.Now()
				// schedule the distro
				res := ds.scheduleDistro(d.distroId, d.runnableTasksForDistro, taskExpectedDuration)
				if res.err != nil {
					grip.Error(message.Fields{
						"operation": "scheduling distro",
						"distro":    d.distroId,
						"runner":    RunnerName,
						"error":     res.err.Error(),
					})
				}

				if ctx.Err() != nil {
					return
				}
				// write the results out to a results channel
				distroSchedulerResultChan <- res
				grip.Info(message.Fields{
					"runner":                 RunnerName,
					"distro":                 d.distroId,
					"operation":              "scheduling distro",
					"queue_size":             len(d.runnableTasksForDistro),
					"expected_duration":      res.schedulerEvent.ExpectedDuration,
					"expected_duration_span": res.schedulerEvent.ExpectedDuration.String(),
					"span":     time.Since(distroStartTime).String(),
					"duration": time.Since(distroStartTime),
				})
			}
		}()
	}

	// intialize a map of scheduler events
	schedulerEvents := map[string]event.TaskQueueInfo{}

	// prioritize the tasks, one distro at a time
	taskQueueItems := make(map[string][]model.TaskQueueItem)

	resDoneChan := make(chan struct{})
	catcher := grip.NewSimpleCatcher()
	go func() {
		defer close(resDoneChan)
		for res := range distroSchedulerResultChan {
			if res.err != nil {
				catcher.Add(errors.Wrapf(res.err, "error scheduling tasks on distro %v", res.distroId))
				continue
			}
			schedulerEvents[res.distroId] = res.schedulerEvent
			taskQueueItems[res.distroId] = res.taskQueueItem
		}
	}()

	if ctx.Err() != nil {
		return errors.New("scheduling operations canceled")
	}
	// wait for the distro scheduler goroutines to complete to complete
	wg.Wait()

	// wait group has terminated so scheduler channel can be closed
	close(distroSchedulerResultChan)

	// wait for the results to be collected
	<-resDoneChan

	if catcher.HasErrors() {
		return catcher.Resolve()
	}

	totalQueueSize := 0
	for _, queue := range taskQueueItems {
		totalQueueSize += len(queue)
	}

	grip.Info(message.Fields{
		"runner": RunnerName,
		"stat":   "total-queue-size",
		"size":   totalQueueSize,
	})

	// split distros by name
	distrosByName := make(map[string]distro.Distro)
	for _, d := range distros {
		distrosByName[d.Id] = d
	}

	hostPlanningStart := time.Now()

	grip.Notice(message.Fields{
		"runner":    RunnerName,
		"operation": "removing stale intent hosts older than 3 minutes",
	})

	if err = host.RemoveStaleInitializing(""); err != nil {
		return errors.Wrap(err, "problem removing previously intented hosts, before creating new ones.") // nolint:misspell
	}

	// get hosts that we can use
	hostsByDistro, err := findUsableHosts("")
	if err != nil {
		return err
	}

	// add the length of the host lists of hosts that are running to the event log.
	for distroId, hosts := range hostsByDistro {
		taskQueueInfo := schedulerEvents[distroId]
		taskQueueInfo.NumHostsRunning = len(hosts)
		schedulerEvents[distroId] = taskQueueInfo
	}
	grip.Info(message.Fields{
		"runner":    RunnerName,
		"operation": "host query and processing",
		"span":      time.Since(hostPlanningStart).String(),
		"duration":  time.Since(hostPlanningStart),
	})

	// construct the data that will be needed by the host allocator
	hostAllocatorData := HostAllocatorData{
		existingDistroHosts:  hostsByDistro,
		distros:              distrosByName,
		taskQueueItems:       taskQueueItems,
		taskRunDistros:       taskRunDistros,
		projectTaskDurations: taskExpectedDuration,
	}

	// figure out how many new hosts we need
	hs := &hostScheduler{
		HostAllocator: s.HostAllocator,
	}

	newHostsNeeded, err := hs.NewHostsNeeded(ctx, hostAllocatorData)
	if err != nil {
		return errors.Wrap(err, "Error determining how many new hosts are needed")
	}

	// spawn up the hosts
	hostsSpawned, err := hs.spawnHosts(ctx, newHostsNeeded)
	if err != nil {
		return errors.Wrap(err, "Error spawning new hosts")
	}

	grip.Info(message.Fields{
		"message":     "hosts spawned",
		"num_distros": len(hostsSpawned),
		"runner":      RunnerName,
		"allocations": newHostsNeeded,
	})

	for distro, hosts := range hostsSpawned {
		taskQueueInfo := schedulerEvents[distro]
		taskQueueInfo.NumHostsRunning += len(hosts)
		schedulerEvents[distro] = taskQueueInfo

		hostList := make([]string, len(hosts))
		for idx, host := range hosts {
			hostList[idx] = host.Id
		}

		if ctx.Err() != nil {
			return errors.New("scheduling run canceled")
		}

		makespan := taskQueueInfo.ExpectedDuration / time.Duration(len(hostsByDistro)+len(hostsSpawned))
		grip.Info(message.Fields{
			"runner":             RunnerName,
			"distro":             distro,
			"new_hosts":          hostList,
			"num":                len(hostList),
			"queue":              taskQueueInfo,
			"total_runtime":      taskQueueInfo.ExpectedDuration.String(),
			"predicted_makespan": makespan.String(),
			"event":              taskQueueInfo,
		})
	}

	for d, t := range schedulerEvents {
		event.LogSchedulerEvent(event.SchedulerEventData{
			TaskQueueInfo: t,
			DistroId:      d,
		})
	}

	grip.Info(message.Fields{
		"runner":    RunnerName,
		"operation": "total host planning",
		"span":      time.Since(hostPlanningStart).String(),
		"duration":  time.Since(hostPlanningStart),
	})

	return nil
}

type distroSchedulerInput struct {
	distroId               string
	runnableTasksForDistro []task.Task
}

type distroSchedulerResult struct {
	distroId       string
	schedulerEvent event.TaskQueueInfo
	taskQueueItem  []model.TaskQueueItem
	err            error
}

type distroSchedueler struct {
	TaskPrioritizer
	TaskQueuePersister
}

func (s *distroSchedueler) scheduleDistro(distroId string, runnableTasksForDistro []task.Task,
	taskExpectedDuration model.ProjectTaskDurations) distroSchedulerResult {

	res := distroSchedulerResult{
		distroId: distroId,
	}
	grip.Info(message.Fields{
		"runner":    RunnerName,
		"distro":    distroId,
		"num_tasks": len(runnableTasksForDistro),
	})

	prioritizedTasks, err := s.PrioritizeTasks(distroId, runnableTasksForDistro)
	if err != nil {
		res.err = errors.Wrap(err, "Error prioritizing tasks")
		return res
	}

	// persist the queue of tasks
	grip.Debug(message.Fields{
		"runner":    RunnerName,
		"distro":    distroId,
		"operation": "saving task queue for distro",
	})

	queuedTasks, err := s.PersistTaskQueue(distroId, prioritizedTasks,
		taskExpectedDuration)
	if err != nil {
		res.err = errors.Wrapf(err, "Error processing distro %s saving task queue", distroId)
		return res
	}

	// track scheduled time for prioritized tasks
	err = task.SetTasksScheduledTime(prioritizedTasks, time.Now())
	if err != nil {
		res.err = errors.Wrapf(err,
			"Error processing distro %s setting scheduled time for prioritized tasks",
			distroId)
		return res
	}
	res.taskQueueItem = queuedTasks

	var totalDuration time.Duration
	for _, item := range queuedTasks {
		totalDuration += item.ExpectedDuration
	}
	// initialize the task queue info
	res.schedulerEvent = event.TaskQueueInfo{
		TaskQueueLength:  len(queuedTasks),
		NumHostsRunning:  0,
		ExpectedDuration: totalDuration,
	}

	// final sanity check
	if len(runnableTasksForDistro) != len(res.taskQueueItem) {
		delta := make(map[string]string)
		for _, t := range res.taskQueueItem {
			delta[t.Id] = "res.taskQueueItem"
		}
		for _, i := range runnableTasksForDistro {
			if delta[i.Id] == "res.taskQueueItem" {
				delete(delta, i.Id)
			} else {
				delta[i.Id] = "d.runnableTasksForDistro"
			}
		}
		grip.Alert(message.Fields{
			"runner":             RunnerName,
			"distro":             distroId,
			"message":            "inconsistency with scheduler input and output",
			"inconsistent_tasks": delta,
		})
	}

	return res

}

// Takes in a version id and a map of "key -> buildvariant" (where "key" is of
// type "versionBuildVariant") and updates the map with an entry for the
// buildvariants associated with "versionStr"
func (s *Scheduler) getProject(versionStr string) (*model.Project, error) {
	version, err := version.FindOne(version.ById(versionStr))
	if err != nil {
		return nil, err
	}
	if version == nil {
		return nil, errors.Errorf("nil version returned for version '%s'", versionStr)
	}

	project := &model.Project{}
	err = model.LoadProjectInto([]byte(version.Config), version.Identifier, project)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to load project config for version %s", versionStr)
	}
	return project, nil
}

func updateVersionBuildVarMap(versionStr string, p *model.Project, versionBuildVarMap map[versionBuildVariant]model.BuildVariant) {
	for _, buildVariant := range p.BuildVariants {
		key := versionBuildVariant{versionStr, buildVariant.Name}
		versionBuildVarMap[key] = buildVariant
	}
}

// Takes in a list of tasks, and splits them by distro.
// Returns a map of distro name -> tasks that can be run on that distro
// and a map of task id -> distros that the task can be run on (for tasks
// that can be run on multiple distro)
func (s *Scheduler) splitTasksByDistro(tasksToSplit []task.Task) (
	map[string][]task.Task, map[string][]string, error) {
	tasksByDistro := make(map[string][]task.Task)
	taskRunDistros := make(map[string][]string)

	// map of versionBuildVariant -> build variant
	versionBuildVarMap := make(map[versionBuildVariant]model.BuildVariant)

	// insert the tasks into the appropriate distro's queue in our map
	for _, t := range tasksToSplit {
		key := versionBuildVariant{t.Version, t.BuildVariant}
		var p *model.Project
		var err error
		if _, exists := versionBuildVarMap[key]; !exists {
			p, err = s.getProject(t.Version)
			if err != nil {
				grip.Info(message.WrapError(err, message.Fields{
					"runner":  RunnerName,
					"version": t.Version,
					"task":    t.Id,
					"message": "skipping version after problem getting project for task",
					"err":     errors.WithStack(err),
				}))
				continue
			}
			updateVersionBuildVarMap(t.Version, p, versionBuildVarMap)
		}

		// get the build variant for the task
		buildVariant, ok := versionBuildVarMap[key]
		if !ok {
			grip.Info(message.Fields{
				"runner":  RunnerName,
				"variant": t.BuildVariant,
				"project": t.Project,
				"task":    t.Id,
				"message": "buildvariant not defined",
			})
			continue
		}

		distros, err := s.getDistrosForBuildVariant(t, buildVariant, p)
		// If no matching spec was found, log it and continue.
		if err != nil {
			grip.Info(message.Fields{
				"runner":  RunnerName,
				"variant": t.BuildVariant,
				"project": t.Project,
				"task":    t.Id,
				"message": "task has no matching spec for buildvariant",
			})
			continue
		}

		// use the specified distros for the task, or, if none are specified,
		// the default distros for the build variant
		distrosToUse := buildVariant.RunOn
		if len(distros) != 0 {
			distrosToUse = distros
		}
		// remove duplicates to avoid scheduling twice
		distrosToUse = util.UniqueStrings(distrosToUse)
		for _, d := range distrosToUse {
			tasksByDistro[d] = append(tasksByDistro[d], t)
		}

		if t.DistroId == "" {
			// this is a lazy way to backfill distro names on tasks.
			if err = t.SetDistro(distrosToUse[0]); err != nil {
				grip.Info(message.WrapError(err, message.Fields{
					"runner":  RunnerName,
					"version": t.Version,
					"task":    t.Id,
					"distro":  distrosToUse[0],
					"message": "failed to backfill task distro",
					"err":     errors.WithStack(err),
				}))
				continue
			}
		}

		// for tasks that can run on multiple distros, keep track of which
		// distros they will be scheduled on
		if len(distrosToUse) > 1 {
			taskRunDistros[t.Id] = distrosToUse
		}
	}

	return tasksByDistro, taskRunDistros, nil
}

func (s *Scheduler) getDistrosForBuildVariant(task task.Task, bv model.BuildVariant, p *model.Project) ([]string, error) {
	for _, bvTask := range bv.Tasks {
		if bvTask.Name == task.DisplayName { // task is listed in buildvariant
			return bvTask.Distros, nil
		}
	}

	if p == nil {
		var err error
		p, err = s.getProject(task.Version)
		if err != nil {
			return []string{}, errors.New("error finding project for task")
		}
	}
	taskGroups := map[string][]string{}
	for _, tg := range p.TaskGroups {
		taskGroups[tg.Name] = tg.Tasks
	}
	for _, bvTask := range bv.Tasks {
		if tasksInTaskGroup, ok := taskGroups[bvTask.Name]; ok {
			for _, t := range tasksInTaskGroup {
				if t == task.DisplayName { // task is listed in task group
					return bvTask.Distros, nil
				}
			}
		}
	}

	return []string{}, errors.New("no matching task found for buildvariant")
}

type hostScheduler struct {
	HostAllocator
}

// Call out to the embedded CloudManager to spawn hosts.  Takes in a map of
// distro -> number of hosts to spawn for the distro.
// Returns a map of distro -> hosts spawned, and an error if one occurs.
func (s *hostScheduler) spawnHosts(ctx context.Context, newHostsNeeded map[string]int) (map[string][]host.Host, error) {
	startTime := time.Now()

	// loop over the distros, spawning up the appropriate number of hosts
	// for each distro
	hostsSpawnedPerDistro := make(map[string][]host.Host)
	for distroId, numHostsToSpawn := range newHostsNeeded {
		distroStartTime := time.Now()

		if numHostsToSpawn == 0 {
			continue
		}

		hostsSpawnedPerDistro[distroId] = make([]host.Host, 0, numHostsToSpawn)
		for i := 0; i < numHostsToSpawn; i++ {
			if ctx.Err() != nil {
				return nil, errors.New("scheduling run canceled.")
			}

			d, err := distro.FindOne(distro.ById(distroId))
			if err != nil {
				grip.Error(message.WrapError(err, message.Fields{
					"distro":  distroId,
					"runner":  RunnerName,
					"message": "failed to find distro",
				}))
				continue
			}

			allDistroHosts, err := host.Find(host.ByDistroId(distroId))
			if err != nil {
				grip.Error(message.WrapError(err, message.Fields{
					"distro":  distroId,
					"runner":  RunnerName,
					"message": "problem getting hosts for distro",
				}))

				continue
			}

			if len(allDistroHosts) >= d.PoolSize {
				grip.Info(message.Fields{
					"distro":    distroId,
					"runner":    RunnerName,
					"pool_size": d.PoolSize,
					"message":   "max hosts running",
				})

				continue
			}

			hostOptions := cloud.HostOptions{
				UserName: evergreen.User,
				UserHost: false,
			}

			intentHost := cloud.NewIntent(d, d.GenerateName(), d.Provider, hostOptions)
			if err := intentHost.Insert(); err != nil {
				err = errors.Wrapf(err, "Could not insert intent host '%s'", intentHost.Id)

				grip.Error(message.WrapError(err, message.Fields{
					"distro":   distroId,
					"runner":   RunnerName,
					"host":     intentHost.Id,
					"provider": d.Provider,
				}))

				return nil, err
			}

			hostsSpawnedPerDistro[distroId] = append(hostsSpawnedPerDistro[distroId], *intentHost)

		}
		// if none were spawned successfully
		if len(hostsSpawnedPerDistro[distroId]) == 0 {
			delete(hostsSpawnedPerDistro, distroId)
		}

		grip.Info(message.Fields{
			"runner":    RunnerName,
			"distro":    distroId,
			"number":    numHostsToSpawn,
			"operation": "spawning instances",
			"span":      time.Since(distroStartTime).String(),
			"duration":  time.Since(distroStartTime),
		})
	}

	grip.Info(message.Fields{
		"runner":    RunnerName,
		"operation": "host query and processing",
		"span":      time.Since(startTime).String(),
		"duration":  time.Since(startTime),
	})

	return hostsSpawnedPerDistro, nil
}

// Finds live hosts in the DB and organizes them by distro. Pass the
// empty string to retrieve all distros
func findUsableHosts(distroID string) (map[string][]host.Host, error) {
	// fetch all hosts, split by distro
	query := host.IsLive()
	if distroID != "" {
		key := bsonutil.GetDottedKeyName(host.DistroKey, distro.IdKey)
		query[key] = distroID
	}

	allHosts, err := host.Find(db.Query(query))
	if err != nil {
		return nil, errors.Wrap(err, "Error finding live hosts")
	}

	// figure out all hosts we have up - per distro
	hostsByDistro := make(map[string][]host.Host)
	for _, liveHost := range allHosts {
		hostsByDistro[liveHost.Distro.Id] = append(hostsByDistro[liveHost.Distro.Id],
			liveHost)
	}

	return hostsByDistro, nil
}

// pass 'allDistros' or the empty string to unchedule all distros.
func underwaterUnschedule(distroID string) error {
	if underwaterPruningEnabled {
		num, err := task.UnscheduleStaleUnderwaterTasks(distroID)
		if err != nil {
			return errors.WithStack(err)
		}

		grip.InfoWhen(num > 0, message.Fields{
			"message": "unscheduled stale tasks",
			"runner":  RunnerName,
			"count":   num,
		})
	}

	return nil
}
