package reactor

import (
	"context"
	"errors"
	"fmt"
	"github.com/moontrade/kirana/pkg/counter"
	"github.com/moontrade/kirana/pkg/hashmap"
	"github.com/moontrade/kirana/pkg/mpmc"
	"github.com/moontrade/kirana/pkg/pmath"
	"github.com/moontrade/kirana/pkg/runtimex"
	"github.com/moontrade/kirana/pkg/timex"
	"github.com/moontrade/kirana/pkg/util"
	"github.com/moontrade/kirana/pkg/wyhash"
	"github.com/panjf2000/ants"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type Stats struct {
	ticks              counter.Counter
	ticksDur           counter.TimeCounter
	ticksDurMin        counter.Counter
	ticksDurMax        counter.Counter
	spawns             counter.Counter
	spawnsDur          counter.TimeCounter
	wakes              counter.Counter
	wakesDur           counter.TimeCounter
	wakeLists          counter.Counter
	wakesListsDur      counter.TimeCounter
	wakeListsWakes     counter.Counter
	wakesListsWakesDur counter.TimeCounter
	wakeListsInvokes   counter.Counter
	invokes            counter.Counter
	invokesDur         counter.TimeCounter
	flushes            counter.Counter
	flushesDur         counter.TimeCounter
	skew               counter.Counter
	skewDur            counter.TimeCounter
	droppedDur         counter.TimeCounter
	level1Ticks        counter.Counter
	level1TicksDur     counter.TimeCounter
	level1TicksDurMin  counter.Counter
	level1TicksDurMax  counter.Counter
	level2Ticks        counter.Counter
	level2TicksDur     counter.TimeCounter
	level2TicksDurMin  counter.Counter
	level2TicksDurMax  counter.Counter
	level3Ticks        counter.Counter
	level3TicksDur     counter.TimeCounter
	level3TicksDurMin  counter.Counter
	level3TicksDurMax  counter.Counter
	pidSwitches        counter.Counter
}

type Runnable interface{}

var (
	ErrQueueFull = errors.New("queue full")
	ErrStop      = errors.New("stop")
)

const (
	DefaultInvokeQueueSize = 1024 * 1
	DefaultWakeQueueSize   = 1024 * 1
	DefaultSpawnQueueSize  = 1024 * 1
)

type Config struct {
	Name         string
	Level1Wheel  Wheel
	Level2Wheel  Wheel
	Level3Wheel  Wheel
	InvokeQSize  int
	WakeQSize    int
	SpawnQSize   int
	LockOSThread bool
}

// Reactor runs all tasks on a single goroutine. It has an optimized timing mechanism
// with a fixed tickDur duration and a fixed interval duration. The interval is broken down
// into slots for better resource allocation. For example, a tickDur duration of 4ms with 5
// slots gives an interval duration of 20ms with each 4ms handling ~20% of the load. The
// timing is constantly adjusting to ensure the tickDur duration is accurate from the start
// adjusting for CPU Time.
// In addition, there is a lock-free MPMC queue that accepts invokes to run immediately
// without having to wait for a Tick.
type Reactor struct {
	Stats
	id   int
	now  int64
	size counter.Counter
	//currentTick    counter.Counter
	idCounter      counter.Counter
	state          int64
	config         Config
	wakeQ          *mpmc.BoundedWake[Task]
	wakeListQ      *mpmc.BoundedWake[WakeList]
	spawnQ         *mpmc.BoundedWake[Task]
	invokeQ        *mpmc.BoundedWake[func()]
	timer          chan Tick
	lastTick       int64
	currentTick    int64
	tickWheel      Wheel
	level2Wheel    Wheel
	level3Wheel    Wheel
	tickDur        time.Duration
	ticksPerLevel2 int64
	ticksPerLevel3 int64
	tasks          *hashmap.SyncMap[int64, *Task]
	ctx            context.Context
	cancel         context.CancelFunc
	tickCount      counter.Counter
	nextTick       counter.Counter
	wakeCh         chan int64
	pid            int32
	gid            uint64
	wg             sync.WaitGroup
}

func NewReactor(config Config) (*Reactor, error) {
	if config.Name == "" {
		config.Name = "loop"
	}
	if config.InvokeQSize <= 4 {
		config.InvokeQSize = DefaultInvokeQueueSize
	}
	if config.WakeQSize <= 4 {
		config.WakeQSize = DefaultWakeQueueSize
	}
	if config.SpawnQSize <= 4 {
		config.SpawnQSize = DefaultSpawnQueueSize
	}
	config.InvokeQSize = pmath.CeilToPowerOf2(config.InvokeQSize)
	config.WakeQSize = pmath.CeilToPowerOf2(config.WakeQSize)
	config.SpawnQSize = pmath.CeilToPowerOf2(config.SpawnQSize)
	if len(config.Level1Wheel.durations) == 0 {
		config.Level1Wheel = NewWheel(Millis250)
	}
	if len(config.Level2Wheel.durations) == 0 {
		config.Level2Wheel = NewWheel(Seconds)
	}
	if len(config.Level3Wheel.durations) == 0 {
		config.Level3Wheel = NewWheel(Minutes)
	}
	if config.Level2Wheel.tickDur%config.Level1Wheel.tickDur != 0 {
		return nil, fmt.Errorf("seconds Tick not evenly divisible by millisecond Tick: %s mod %s = %s",
			config.Level2Wheel.tickDur, config.Level1Wheel.tickDur, config.Level2Wheel.tickDur%config.Level1Wheel.tickDur)
	}
	if config.Level3Wheel.tickDur%config.Level1Wheel.tickDur != 0 {
		return nil, fmt.Errorf("minutes Tick not evenly divisible by millisecond Tick: %s mod %s = %s",
			config.Level3Wheel.tickDur, config.Level1Wheel.tickDur, config.Level3Wheel.tickDur%config.Level1Wheel.tickDur)
	}
	wakeCh := make(chan int64, 1)
	ctx, cancel := context.WithCancel(context.Background())
	w := &Reactor{
		config:         config,
		tickDur:        config.Level1Wheel.tickDur,
		tickWheel:      config.Level1Wheel,
		level2Wheel:    config.Level2Wheel,
		ticksPerLevel2: int64(config.Level2Wheel.tickDur / config.Level1Wheel.tickDur),
		level3Wheel:    config.Level3Wheel,
		ticksPerLevel3: int64(config.Level3Wheel.tickDur / config.Level1Wheel.tickDur),
		wakeCh:         wakeCh,
		tasks:          hashmap.NewSyncMap[int64, *Task](8, 1024, wyhash.Int64),
		wakeQ:          mpmc.NewBoundedWake[Task](int64(config.WakeQSize), wakeCh),
		wakeListQ:      mpmc.NewBoundedWake[WakeList](int64(config.WakeQSize), wakeCh),
		spawnQ:         mpmc.NewBoundedWake[Task](int64(config.SpawnQSize), wakeCh),
		invokeQ:        mpmc.NewBoundedWake[func()](int64(config.InvokeQSize), wakeCh),
		timer:          make(chan Tick, 1),
		ctx:            ctx,
		cancel:         cancel,
	}
	w.id = reactors.AppendIndex(w)
	return w, nil
}

func (r *Reactor) CheckGID() bool {
	return r.gid == runtimex.GoroutineID()
}

func (r *Reactor) ID() int { return r.id }

func (r *Reactor) Now() int64 { return r.now }

func (r *Reactor) SnapshotStats() Stats {
	return r.Stats
}

func (r *Reactor) Start() {
	if !atomic.CompareAndSwapInt64(&r.state, 0, 1) {
		return
	}
	go r.run()
}

func (r *Reactor) Duration(ticks int64) time.Duration {
	return r.tickDur * time.Duration(ticks)
}

func (r *Reactor) Ticks(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64(duration) / int64(r.tickDur)
}

func (r *Reactor) Wake(task *Task) error {
	if task == nil {
		return errors.New("task is nil")
	}
	reactor := task.reactor
	if reactor == nil {
		return errors.New("task is not scheduled")
	}
	if reactor != r {
		return reactor.Wake(task)
	}
	r.wakeQ.Enqueue(task)
	return nil
}

func (r *Reactor) WakeAfter(task *Task, after time.Duration) error {
	if task == nil {
		return errors.New("task is nil")
	}
	if after <= 0 {
		return r.Wake(task)
	}
	reactor := task.reactor
	if reactor == nil {
		return errors.New("task is not scheduled")
	}
	task.wakeAfter = after
	if reactor != r {
		return reactor.Wake(task)
	}
	if !r.wakeQ.Enqueue(task) {
		return ErrQueueFull
	} else {
		return nil
	}
}

func (r *Reactor) wakeList(list *WakeList) error {
	if list == nil {
		return errors.New("nil slots")
	}
	if list.Len() == 0 {
		return nil
	}
	if list.reactor != r {
		return list.reactor.wakeList(list)
	}
	if !r.wakeListQ.Enqueue(list) {
		return ErrQueueFull
	} else {
		return nil
	}
}

func (r *Reactor) Invoke(fn func()) bool {
	if fn == nil {
		return false
	}
	return r.invokeQ.EnqueueUnsafeTimeout(runtimex.FuncToPointer(fn), time.Second*5)
}

func (r *Reactor) InvokeRef(fn *func()) bool {
	if fn == nil {
		return false
	}
	return r.invokeQ.EnqueueUnsafe(runtimex.FuncToPointer(*fn))
}

func (r *Reactor) InvokeBlocking(fn func()) bool {
	if fn == nil {
		return false
	}
	return EnqueueBlocking(fn)
}

func (r *Reactor) Spawn(future Future) (*Task, error) {
	if future == nil {
		return nil, errors.New("nil future")
	}
	task := taskPool.Get()
	task.init(r.idCounter.Incr(), r, future)
	if provider, ok := future.(FutureTask); ok {
		provider.SetTask(task)
	}
	if !r.spawnQ.Enqueue(task) {
		return nil, ErrQueueFull
	}
	return task, nil
}

func (r *Reactor) SpawnInterval(future Future, interval time.Duration) (*Task, error) {
	if future == nil {
		return nil, errors.New("nil future")
	}
	if interval < 0 {
		interval = 0
	}
	task := taskPool.Get()
	task.init(r.idCounter.Incr(), r, future)
	task.interval = interval
	if provider, ok := future.(FutureTask); ok {
		provider.SetTask(task)
	}
	if !r.spawnQ.Enqueue(task) {
		return nil, ErrQueueFull
	}
	return task, nil
}

func (r *Reactor) SpawnWorkerFn(fn func()) error {
	return ants.Submit(fn)
}

func (r *Reactor) run() {
	defer func() {
		e := recover()
		if e != nil {
			//logger.Error(util.PanicToError(e))
		}
	}()
	if r.config.LockOSThread {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}

	r.gid, r.pid = runtimex.GIDPID()
	tick, err := initTicker(r.tickDur).Register(r.tickDur, r, r.wakeCh)
	if err != nil {
		panic(err)
	}
	defer func() {
		_ = tick.Close()
	}()
	for {
		select {
		case v := <-r.wakeCh:
			r.onWakeMessage(v)
		}
	}
}

func (r *Reactor) onWakeMessage(v int64) {
	r.maybeProcessTick(v)
	r.now = timex.NanoTime()
	for r.flushQueues() > 0 {
		r.now = timex.NanoTime()
	}
}

func (r *Reactor) maybeProcessTick(v int64) {
	if v < 1 || v == r.lastTick {
		return
	}
	r.currentTick = v
	if r.lastTick < v-1 {
		r.catchup(r.lastTick, r.currentTick)
	}
	r.lastTick = r.currentTick
	r.processTick(r.currentTick)
}

func (r *Reactor) processTick(tick int64) {
	interval := int64(r.tickDur)
	start := timex.NanoTime()
	begin := start
	r.tick(tick, begin)
	end := timex.NanoTime()
	elapsed := end - begin

	// Stats
	r.ticks.Incr()
	r.ticksDur.Add(elapsed)
	if r.ticksDurMin == 0 || r.ticksDurMin.Load() > elapsed {
		r.ticksDurMin.Store(elapsed)
	}
	if r.ticksDurMax.Load() < elapsed {
		r.ticksDurMax.Store(elapsed)
	}

	begin = end
	r.flushQueues()
	end = timex.NanoTime()
	r.flushesDur.Add(end - begin)
	elapsed = end - start
	r.flushes.Add(1)

	if elapsed > interval {
		r.skew.Incr()
		r.skewDur.Add(elapsed)
		r.rebalance()
	}
}

func (r *Reactor) onSpawn(task *Task) {
	r.pollStart(r.now, task)
}

func (r *Reactor) onWake(task *Task) {
	r.pollWake(r.now, task)
}

func (r *Reactor) onWakeList(list *WakeList) {
	r.pollWakeList(r.now, list)
}

func (r *Reactor) onFn(task func()) {
	r.invoke(task)
}

func (r *Reactor) flushQueues() int {
	total := 0
	if !r.wakeListQ.IsEmpty() {
		count := r.wakeListQ.DequeueMany(math.MaxUint32, r.onWakeList)
		total += count
		r.wakeLists.Add(int64(count))
	}
	if !r.invokeQ.IsEmpty() {
		count := r.invokeQ.DequeueManyDeref(math.MaxUint32, r.onFn)
		total += count
		r.invokes.Add(int64(count))
	}
	if !r.wakeQ.IsEmpty() {
		count := r.wakeQ.DequeueMany(math.MaxUint32, r.onWake)
		total += count
		r.wakes.Add(int64(count))
	}
	if !r.spawnQ.IsEmpty() {
		count := r.spawnQ.DequeueMany(math.MaxUint32, r.onSpawn)
		total += count
		r.spawns.Add(int64(count))
	}
	return total
}

func (r *Reactor) catchup(lastTick, currentTick int64) {
	//logger.Warn("skew detected of %d ticks", currentTick-1-lastTick)
	println("skew detected of %d ticks", currentTick-1-lastTick)
	//logger.Warn("catching up...")

	now := timex.NanoTime()
	for nextTick := lastTick + 1; nextTick <= currentTick; nextTick++ {
		r.tick(nextTick, now)
	}
}

func (r *Reactor) rebalance() {
	//logger.Warn("rebalancing...")
	//logger.Warn("rebalanced")
}

func (r *Reactor) tick(tick int64, now int64) {
	r.tickWheel.tick(now, r.onTick)
	if tick%r.ticksPerLevel2 == 0 {
		//logger.Debug("level 2 wheel Tick", tick)
		r.level2Wheel.tick(now, r.onTick)
	}
	if tick%r.ticksPerLevel3 == 0 {
		//logger.Debug("level 3 wheel Tick")
		r.level3Wheel.tick(now, r.onTick)
	}
}

func (r *Reactor) onTick(now int64, list *taskSwapList, slot *taskSwapSlot, task *Task) bool {
	if slot.wake {
		r.pollWake(now, task)
		return false
	}
	return r.pollInterval(now, list, task)
}

func (r *Reactor) schedule(task *Task, delay time.Duration, wake bool) {
	if delay < r.tickWheel.maxDur && r.tickWheel.tickDur > 0 {
		r.tickWheel.schedule(task, delay, wake)
	} else if delay < r.level2Wheel.maxDur && r.level2Wheel.tickDur > 0 {
		r.level2Wheel.schedule(task, delay, wake)
	} else if delay < r.level3Wheel.maxDur && r.level3Wheel.tickDur > 0 {
		r.level3Wheel.schedule(task, delay, wake)
	}
}

func (r *Reactor) stopTask(time int64, task *Task) {
	defer func() {
		task.remove()
		if e := recover(); e != nil {
			err := util.PanicToError(e)
			_ = err
			//logger.Error(err, "Reactor.invoke panic")
		}
	}()
	_, ok := r.tasks.Delete(task.id)
	if !ok {
		return
	}
	task.stop = true
	task.clearSlots()

	if pc, ok := task.future.(PollClose); ok {
		err := pc.PollClose(CloseEvent{
			Task:   task,
			Time:   time,
			Reason: nil,
		})
		if err != nil {
			//logger.Warn(err)
		}
	}
}

func (r *Reactor) invoke(fn func()) {
	defer func() {
		if e := recover(); e != nil {
			err := util.PanicToError(e)
			_ = err
			//logger.Error(err, "Reactor.invoke panic")
		}
	}()
	if fn != nil {
		fn()
	}
}

func (r *Reactor) pollStart(now int64, task *Task) {
	defer func() {
		if e := recover(); e != nil {
			err := util.PanicToError(e)
			_ = err
			//logger.Error(err, "Reactor.pollInvoke panic")
		}
	}()
	task.started = now
	err := task.future.Poll(Context{
		Task:   task,
		Time:   now,
		Reason: ReasonStart,
	})

	if err != nil {
		if err == ErrStop {
			task.stop = true
		} else {
			//logger.Warn(err)
		}
	}

	if task.stop {
		r.stopTask(now, task)
		return
	}

	r.tasks.Put(task.id, task)

	if task.wakeAfter > 0 {
		r.schedule(task, task.wakeAfter, true)
	}

	if task.interval > 0 {
		r.schedule(task, task.interval, false)
	}
}

func (r *Reactor) pollWakeList(now int64, list *WakeList) {
	//for list.onWake(now) > 0 {
	list.onWake(now)
	count := list.wakes.wake(now, r.onTaskSlotWake)
	r.wakeListsWakes.Add(count)
	count = list.funcs.wake(now, r.onFuncSlotWake)
	r.wakeListsInvokes.Add(count)
	//}
}

func (r *Reactor) onTaskSlotWake(slot *TaskSlot) {
	t := slot.task
	if t == nil {
		return
	}
	r.pollWake(r.now, t)
}

func (r *Reactor) onFuncSlotWake(slot *FuncSlot) {
	fn := slot.Value
	if fn == nil {
		return
	}
	r.invoke(fn)
}

func (r *Reactor) pollWake(now int64, task *Task) {
	if task.stop {
		return
	}
	defer func() {
		if e := recover(); e != nil {
			err := util.PanicToError(e)
			_ = err
			//logger.Error(err, "Reactor.pollInvoke panic")
		}
	}()

	interval := task.interval
	wakeAfter := task.wakeAfter
	if wakeAfter > 0 {
		task.wakeAfter = 0
		r.schedule(task, wakeAfter, true)
		return
	}

	task.wakes++
	err := task.future.Poll(Context{
		Task:   task,
		Time:   now,
		Reason: ReasonWake,
	})

	if err != nil {
		if err == ErrStop {
			task.stop = true
		} else {
			//logger.Warn(err)
		}
	}

	if task.stop {
		r.stopTask(now, task)
		return
	}

	newWakeAfter := task.wakeAfter
	if newWakeAfter != wakeAfter {
		if newWakeAfter > 0 {
			r.schedule(task, newWakeAfter, true)
		}
	}

	nextInterval := task.interval
	// Interval change requested?
	if nextInterval != interval {
		if nextInterval <= 0 {
			task.interval = 0
		}
	}
}

func (r *Reactor) pollInterval(now int64, list *taskSwapList, task *Task) bool {
	if task.stop || task.interval == 0 || list.dur != task.interval {
		// remove
		return false
	}

	defer func() {
		if e := recover(); e != nil {
			err := util.PanicToError(e)
			_ = err
			//logger.Error(err, "Reactor.pollInterval panic")
		}
	}()

	interval := task.interval
	wakeAfter := task.wakeAfter

	task.intervals++
	err := task.future.Poll(Context{
		Task:     task,
		Time:     now,
		Interval: interval,
		Reason:   ReasonInterval,
	})

	if err != nil {
		if err == ErrStop {
			task.stop = true
		} else {
			//logger.Warn(err)
		}
	}

	if task.stop {
		r.stopTask(now, task)
		// remove
		return false
	}

	newWakeAfter := task.wakeAfter
	if newWakeAfter != wakeAfter {
		if newWakeAfter > 0 {
			r.schedule(task, newWakeAfter, true)
		}
	}

	// Interval change requested?
	nextInterval := task.interval
	if nextInterval != interval {
		if nextInterval <= 0 {
			task.interval = 0
			return false
		}
		// Schedule new interval
		r.schedule(task, nextInterval, false)
		// Remove from this taskSwapList
		return false
	}
	return true
}

func (r *Reactor) Print() {
	avg := time.Duration(r.ticksDur.Load()) / time.Duration(r.currentTick)
	fmt.Println("Size			", r.size.Load())
	fmt.Println("ProcessorID				", r.pid)
	fmt.Println("ProcessorID Switches	", r.pidSwitches.Load())
	//fmt.Println("Capacity		", r.cap)
	fmt.Println("Ticks			", r.currentTick)
	//fmt.Println("Ticks Dur 		", Time.Duration(r.ticksDur.Load()))
	fmt.Println("Tick Avg Dur 	", time.Duration(r.ticksDur.Load())/time.Duration(r.currentTick))
	//fmt.Println("Skew			", r.skew.Load())
	//fmt.Println("Skew Dur		", Time.Duration(r.skewDur.Load()))
	//fmt.Println("Dropped Dur	", r.droppedDur.Load())
	////fmt.Println("Ticks Dur 		", Time.Duration(r.ticksDur.Load()))
	//fmt.Println("Jobs			", r.invokes.Load())
	////fmt.Println("Ticks Dur 		", Time.Duration(r.ticksDur.Load()))
	//fmt.Println("Jobs Avg Dur 	", Time.Duration(r.invokesDur.Load())/Time.Duration(r.invokes.Load()))
	//fmt.Println("Interval 		", r.tickDur)
	fmt.Println("Tick CPU 		", float64(avg)/float64(r.tickDur))
	fmt.Println("Min 	 		", time.Duration(r.ticksDurMin.Load()))
	fmt.Println("Max 	 		", time.Duration(r.ticksDurMax.Load()))
	//for i, slots := range r.tickWheel {
	//	fmt.Println("Ring: ", i, " Size: ", slots.activeSize)
	//}
}
