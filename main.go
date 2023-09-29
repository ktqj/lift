package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

func CleanupAttrs(groups []string, a slog.Attr) slog.Attr {
	if (a.Key == slog.TimeKey || a.Key == slog.LevelKey) && len(groups) == 0 {
		return slog.Attr{}
	}
	return a
}
var log *slog.Logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: CleanupAttrs}))

type liftStatus int
func (s liftStatus) String() string {
	if s == UP {
		return "UP"
	} else if s == DOWN {
		return "DOWN"
	} else if s == IDLE {
		return "IDLE"
	}
	return "UNKNOWN"
}

const (
	IDLE liftStatus = 0
	DOWN liftStatus = 1
	UP liftStatus = 2

	tickDuration = 100 * time.Millisecond
)

func withoutFirst(slice []int) []int {
	n := slice[:0]
	return append(n, slice[1:]...)
}

type Passenger struct {
	StartingFloor int
	TargetFloor int
	Born int  // world clock's time when spawned
	Pickedup int  // world clock's time when entered a lift
	Delivered int  // world clock's time when delivered
}

func (p Passenger) String() string {
	return fmt.Sprintf("P[%d->%d]", p.StartingFloor, p.TargetFloor)
}

type Lift struct {
	ID int
	Passengers []Passenger
	CurrentFloor int

	backlog []int  // floors to be visited after finishing current run
	destinations []int  // floors to be visited during current run
	status liftStatus
	m sync.Mutex  // lock to protect backlog, destinations and status
}

func (l *Lift) String() string {
	var b strings.Builder
	separator := "|"
	b.WriteString(fmt.Sprintf("Lift #%d", l.ID))
	b.WriteString(separator)
	b.WriteString(l.status.String())
	b.WriteString(separator)
	b.WriteString("[")
	for _, p := range l.Passengers {
		b.WriteString(p.String())
	}
	b.WriteString("]")
	b.WriteString(separator)
	b.WriteString(fmt.Sprintf("%v%s%v", l.destinations, separator, l.backlog))
	return b.String()
}

func (l *Lift) setStatus(status liftStatus) {
	l.status = status
	if l.status == IDLE {
		log.Info(fmt.Sprintf("Lift #%d becomes idle", l.ID))
	} else if l.status == UP {
		log.Info(fmt.Sprintf("Lift #%d starts going up", l.ID))
	} else if l.status == DOWN {
		log.Info(fmt.Sprintf("Lift #%d starts going down", l.ID))
	}
}

func (l *Lift) GetStatus() liftStatus {
	return l.status
}

func (l *Lift) AddToBacklog(floorNumber int) {
	if slices.Index(l.backlog, floorNumber) != -1 {
		return
	}

	l.backlog = append(l.backlog, floorNumber)
	sort.Slice(l.backlog, func(i, j int) bool { return l.backlog[i] < l.backlog[j] })
}

func (l *Lift) AddDestination(floorNumber int) {
	if slices.Index(l.destinations, floorNumber) != -1 {
		return
	}

	l.destinations = append(l.destinations, floorNumber)
	sort.Slice(l.destinations, func(i, j int) bool { return l.destinations[i] < l.destinations[j] })

	if l.status == IDLE {
		if l.CurrentFloor > floorNumber {
			l.setStatus(DOWN)
		} else if l.CurrentFloor < floorNumber{
			l.setStatus(UP)
		}
	}
}

func (l *Lift) Call(floorNumber int) bool {
	//TODO: deciding whether to accept a call should be a part of a pluggable strategy
	l.m.Lock()
	defer l.m.Unlock()

	if l.status == DOWN && floorNumber > l.CurrentFloor {
		return false
	}
	if l.status == UP && floorNumber < l.CurrentFloor {
		return false
	}

	l.AddDestination(floorNumber)
	log.Info(fmt.Sprintf("Lift #%d called to floor %d", l.ID, floorNumber))
	return true
}

func (l *Lift) PressFloorButton(floorNumber int) {
	l.m.Lock()
	defer l.m.Unlock()

	if l.status == DOWN && floorNumber > l.CurrentFloor {
		l.AddToBacklog(floorNumber)
		return
	}

	if l.status == UP && floorNumber < l.CurrentFloor {
		l.AddToBacklog(floorNumber)
		return
	}

	l.AddDestination(floorNumber)
}

func (l *Lift) UpdateRoute() {
	//TODO: finding best route should be a part of a pluggable strategy
	l.m.Lock()
	defer l.m.Unlock()

	if l.status == DOWN {
		l.destinations = l.destinations[:len(l.destinations) - 1]
	} 
	if l.status == UP {
		l.destinations = withoutFirst(l.destinations)
	}
	if l.status == IDLE {
		l.destinations = l.destinations[:0]
	}

	if len(l.destinations) != 0 {
		return
	}

	if len(l.backlog) == 0 {
		l.setStatus(IDLE)
		return
	}

	// change direction to opposite
	l.destinations = append(l.destinations, l.backlog...)
	l.backlog = l.backlog[:0]
	if l.status == UP {
		l.setStatus(DOWN)
	} else if l.status == DOWN {
		l.setStatus(UP)
	}
}

func (l *Lift) CurrentDestination() int {
	l.m.Lock()
	defer l.m.Unlock()

	if l.status == DOWN {
		return l.destinations[len(l.destinations) - 1]
	}
	if l.status == UP {
		return l.destinations[0]
	}
	if l.status == IDLE && len(l.destinations) > 0 {
		return l.destinations[0]
	}
	return -1
}

func (l *Lift) Park(floor *Floor, clock int) {
	log.Info(fmt.Sprintf("Lift #%d opens doors on floor #%d", l.ID, floor.Number))

	staying := l.Passengers[:0]
	for _, p := range l.Passengers {
		if p.TargetFloor != floor.Number {
			staying = append(staying, p)
		} else {
			p.Delivered = clock
			floor.AddDelivered(p)
		}
	}
	log.Info(fmt.Sprintf("Lift #%d unloads %d passengers", l.ID, len(l.Passengers) - len(staying)))
	l.Passengers = staying

	spotsAvailable := cap(l.Passengers) - len(l.Passengers)
	incoming := floor.Unload(spotsAvailable)
	l.Passengers = append(l.Passengers, incoming...)

	log.Info(fmt.Sprintf("Lift #%d loads %d passengers", l.ID, len(incoming)))
	for _, p := range incoming {
		p.Pickedup = clock
		l.PressFloorButton(p.TargetFloor)
	}

	l.UpdateRoute()
	log.Info(l.String())
}

type Floor struct {
	Number int
	Delivered []Passenger
	Waitlist []Passenger
	m sync.Mutex  // protects Waitlist and Delivered
	LiftRequests chan int
}

func (f *Floor) AddPassenger(p Passenger) {
	f.m.Lock()
	defer f.m.Unlock()
	f.Waitlist = append(f.Waitlist, p)
	f.LiftRequests <- f.Number
}

func (f *Floor) AddDelivered(p Passenger) {
	f.m.Lock()
	defer f.m.Unlock()
	f.Delivered = append(f.Delivered, p)
}

func (f *Floor) Unload(count int) []Passenger {
	f.m.Lock()
	defer f.m.Unlock()
	unloaded := make([]Passenger, 0, count)
	if count >= len(f.Waitlist) {
		unloaded = append(unloaded, f.Waitlist...)
		f.Waitlist = f.Waitlist[:0]
	} else {
		unloaded = append(unloaded, f.Waitlist[:count]...)
		left := f.Waitlist[:0]
		left = append(left, f.Waitlist[count:]...)
		f.Waitlist = left
		log.Info(fmt.Sprintf("%d passenger are left on floor #%d", len(left), f.Number))
		f.LiftRequests <- f.Number
	}
	return unloaded
}

type Building struct {
	Clock int // Counts world's ticks, 1 tick is the time required for a lift to move 1 floor
	Floors map[int]*Floor
	Lifts []*Lift
	LiftRequests chan int
	Backlog []int
}

func NewBuilding(ctx context.Context, floorsCount int) *Building {
	liftRequests := make(chan int)
	floors := make(map[int]*Floor, floorsCount)
	for i := 0; i < floorsCount; i++ {
		floors[i] = &Floor{
			Number: i,
			Waitlist: make([]Passenger, 0, 100),
			Delivered: make([]Passenger, 0, 100),
			LiftRequests: liftRequests,
		}
	}

	lifts := []*Lift{
		&Lift{ID: 1, Passengers: make([]Passenger, 0, 10)},
		&Lift{ID: 2, Passengers: make([]Passenger, 0, 10)},
		&Lift{ID: 3, Passengers: make([]Passenger, 0, 10)},
	}

	b := &Building{
		Floors: floors,
		Lifts: lifts,
		LiftRequests: liftRequests,
		Backlog: make([]int, 0, floorsCount),
	}
	return b
}

func (b *Building) ListenCalls(ctx context.Context, caller LiftCaller[*Lift]) {
	ticker := time.NewTicker(tickDuration / 2)
	for {
		select {
		case <-ctx.Done():
			log.Info(fmt.Sprintf("Shutting down lift controller"))
			return
		case <-ticker.C:
			if len(b.Backlog) == 0 {
				continue
			}
			newBacklog := b.Backlog[:0]
			for _, floorNumber := range b.Backlog {
				if ok := caller(floorNumber, b.Lifts...); ok {
					break
				}
				newBacklog = append(newBacklog, floorNumber)
			}
			b.Backlog = newBacklog
			log.Info(fmt.Sprintf("Pending requests: %v", b.Backlog))
		case floorNumber := <-b.LiftRequests:
			log.Info(fmt.Sprintf("Lift requested to floor #%d", floorNumber))
			if slices.Index(b.Backlog, floorNumber) == -1 {
				b.Backlog = append(b.Backlog, floorNumber)
			}
		}
	}
}

func (b *Building) RunLifts(ctx context.Context) {
	clock := time.NewTicker(tickDuration)
	liftClocks := make([]chan struct{}, 0, len(b.Lifts))
	for _, l := range b.Lifts {
		liftClock := make(chan struct{}, 1)
		liftClocks = append(liftClocks, liftClock)
		go b.RunLift(ctx, l, liftClock)
	}
	for {
		select {
		case <-ctx.Done():
			for _, c := range liftClocks {
				close(c)
			}
			return
		case <-clock.C:
			b.Clock++
			for _, c := range liftClocks {
				c <- struct{}{}
			}
		}
	}
}

func (b *Building) RunLift(ctx context.Context, l *Lift, worldClock chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-worldClock:
			if l.status != IDLE {
				log.Info(fmt.Sprintf("Lift #%d arrives at floor #%d", l.ID, l.CurrentFloor))
			}

			if l.CurrentFloor == l.CurrentDestination() {
				l.Park(b.Floors[l.CurrentFloor], b.Clock)
			}

			if l.status == IDLE {
				continue
			}

			if l.status == DOWN {
				l.CurrentFloor--
			} else if l.status == UP {
				l.CurrentFloor++
			}
		}
	}
}

func (b *Building) PrintStats(ctx context.Context) {
	ticker := time.NewTicker(tickDuration + tickDuration / 2)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats.Waiting = 0
			stats.Delivered = 0
			for _, f := range b.Floors {
				stats.Waiting += len(f.Waitlist)
				stats.Delivered += len(f.Delivered)
			}

			stats.Moving = 0
			for _, l := range b.Lifts {
				stats.Moving += len(l.Passengers)
			}
			log.Info(fmt.Sprintf("%+v", stats))
		}
	}
}


func SpawnPassengers(ctx context.Context, b *Building, maxPassengers int) {
	// TODO: sometimes not all passengers are picked up
	ticker := time.NewTicker(tickDuration + tickDuration / 2)
	for {
		select {
		case <-ctx.Done():
			log.Info("Shutting down passenger generation")
			return
		case <-ticker.C:
			spawnFloorNumber := rand.Intn(len(b.Floors))
			targetFloor := rand.Intn(len(b.Floors))
			for {
				if spawnFloorNumber != targetFloor {
					break
				}
				targetFloor = rand.Intn(len(b.Floors))
			}

			p := Passenger{
				StartingFloor: spawnFloorNumber,
				TargetFloor: targetFloor,
				Born: b.Clock,
			}
			log.Info(fmt.Sprintf("Spawning passenger %s", p.String()))
			b.Floors[spawnFloorNumber].AddPassenger(p)
			stats.Spawned++
			if stats.Spawned == maxPassengers {
				return
			}
		}
	}
}

type Stats struct {
	// m sync.Mutex
	Spawned int
	Delivered int
	Waiting int
	Moving int
}
func NewStats() *Stats { return &Stats{} }
var stats *Stats = NewStats()

// ----------------- //
type CallableLift interface {
	Call(floorNumber int) bool
	GetStatus() liftStatus
}

type LiftCaller [T CallableLift] func(floorNumber int, lifts ...T) bool

func SimpleCaller(floorNumber int, lifts ...*Lift) bool {
	// Ask IDLE first, then the rest
	for _, l := range lifts {
		if l.GetStatus() == IDLE {
			if ok := l.Call(floorNumber); ok {
				return true
			}
		}
	}
	for _, l := range lifts {
		if l.GetStatus() != IDLE {
			if ok := l.Call(floorNumber); ok {
				return true
			}
		}
	}
	return false
}
// ------------------- //

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	sigChannel := make(chan os.Signal, 1)
	signal.Notify(sigChannel, os.Interrupt, syscall.SIGTERM)

	floorsCount := 25
	b := NewBuilding(ctx, floorsCount)
	go b.ListenCalls(ctx, SimpleCaller)
	go b.RunLifts(ctx)
	go b.PrintStats(ctx)

	spawnCtx, spawnCancel := context.WithCancel(context.Background())
	maxPassengers := 100
	go SpawnPassengers(spawnCtx, b, maxPassengers)

main_loop:
	for {
		select {
		case <-sigChannel:
			spawnCancel()
			break main_loop
		default:
			if stats.Delivered == maxPassengers {
				break main_loop
			}
			time.Sleep(tickDuration)
		}
	}

	cancel()
	log.Info(fmt.Sprintf("%+v", stats))

	totalWaitTime := 0
	totalMovingTime := 0
	totalFloorsMoved := 0
	for _, f := range b.Floors {
		for _, p := range f.Delivered {
			totalWaitTime += p.Pickedup - p.Born
			totalMovingTime += p.Delivered - p.Pickedup
			floorsMoved := p.TargetFloor - p.StartingFloor
			if floorsMoved < 0 { floorsMoved = -floorsMoved }
			totalFloorsMoved += floorsMoved
		}
	}

	log.Info(fmt.Sprintf(
		"Average passenger waited - %f ticks, was moving - %f ticks, moved - %f floors",
		float64(totalWaitTime) / float64(stats.Spawned),
		float64(totalMovingTime) / float64(stats.Spawned),
		float64(totalFloorsMoved) / float64(stats.Spawned),
	))


}