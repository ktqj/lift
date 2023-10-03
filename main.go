package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"reflect"
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

	tickDuration = 75 * time.Microsecond
)

type Passenger struct {
	StartingFloor int
	TargetFloor int
	Born time.Time
	Pickedup time.Time
	Delivered time.Time
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

func (l *Lift) IsFloorAlongTheWay(floorNumber int) bool {
	if l.status == DOWN && floorNumber < l.CurrentFloor {
		return true
	}
	if l.status == UP && floorNumber > l.CurrentFloor {
		return true
	}
	return false
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
		if l.CurrentFloor >= floorNumber {
			l.setStatus(DOWN)
		} else if l.CurrentFloor < floorNumber{
			l.setStatus(UP)
		}
	}
}

func (l *Lift) Call(floorNumber int) bool {
	l.m.Lock()
	defer l.m.Unlock()

	if l.status == IDLE || l.IsFloorAlongTheWay(floorNumber) {
		log.Info(fmt.Sprintf("Lift #%d accepted call to floor #%d", l.ID, floorNumber))
		l.AddDestination(floorNumber)
		return true
	}
	return false
}

func (l *Lift) PressFloorButton(floorNumber int) {
	l.m.Lock()
	defer l.m.Unlock()

	if l.IsFloorAlongTheWay(floorNumber) {
		l.AddDestination(floorNumber)
	} else {
		l.AddToBacklog(floorNumber)
	}
}

func (l *Lift) UpdateRoute() {
	l.m.Lock()
	defer l.m.Unlock()

	if l.status == DOWN {
		l.destinations = l.destinations[:len(l.destinations) - 1]
	} else if l.status == UP {
		// pop first element
		n := l.destinations[:0]
		l.destinations = append(n, l.destinations[1:]...)
	}else if l.status == IDLE {
		l.destinations = l.destinations[:0]
	}

	if len(l.destinations) != 0 {
		return
	}

	if len(l.backlog) == 0 {
		l.setStatus(IDLE)
		return
	}

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

func (l *Lift) Park(floor *Floor) {
	log.Info(fmt.Sprintf("Lift #%d opens doors on floor #%d", l.ID, floor.Number))
	staying := l.Passengers[:0]
	for _, p := range l.Passengers {
		if p.TargetFloor != floor.Number {
			staying = append(staying, p)
		} else {
			p.Delivered = time.Now()
			floor.AddDelivered(p)
		}
	}
	log.Info(fmt.Sprintf("Lift #%d unloads %d passengers", l.ID, len(l.Passengers) - len(staying)))
	l.Passengers = staying

	spotsAvailable := cap(l.Passengers) - len(l.Passengers)
	incoming := floor.Unload(spotsAvailable)
	for _, p := range incoming {
		p.Pickedup = time.Now()
		l.Passengers = append(l.Passengers, p)
		l.PressFloorButton(p.TargetFloor)
	}

	log.Info(fmt.Sprintf("Lift #%d loads %d passengers", l.ID, len(incoming)))
	l.UpdateRoute()
	log.Info(l.String())
}

func (l *Lift) Run(ctx context.Context, floors map[int]*Floor) {
	ticker := time.NewTicker(tickDuration)
	for {
		select {
		case <-ctx.Done():
			log.Info(fmt.Sprintf("Shutting down lift #%d", l.ID))
			return
		case <-ticker.C:
			if l.status != IDLE {
				log.Info(fmt.Sprintf("Lift #%d arrives at floor #%d", l.ID, l.CurrentFloor))
			}

			if l.CurrentFloor == l.CurrentDestination() {
				l.Park(floors[l.CurrentFloor])
			}

			if l.status == DOWN {
				l.CurrentFloor--
			} else if l.status == UP {
				l.CurrentFloor++
			}
		}
	}
}

type Floor struct {
	Number int
	Delivered []Passenger
	Waitlist []Passenger
	m sync.Mutex  // protects Waitlist and Delivered
	button chan struct{}  // buffered with capacity of 1
}

func NewFloor(floorNumber int) *Floor {
	return &Floor{
		Number: floorNumber,
		Waitlist: make([]Passenger, 0),
		Delivered: make([]Passenger, 0),
		button: make(chan struct{}, 1),
	}
}

func (f *Floor) RequestLift() {
	select {
	case f.button <- struct{}{}:
	default:
	}
}

func (f *Floor) AddPassenger(p Passenger) {
	f.m.Lock()
	defer f.m.Unlock()
	f.Waitlist = append(f.Waitlist, p)
	f.RequestLift()
}

func (f *Floor) AddDelivered(p Passenger) {
	f.m.Lock()
	defer f.m.Unlock()
	f.Delivered = append(f.Delivered, p)
}

func (f *Floor) Unload(count int) []Passenger {
	f.m.Lock()
	defer f.m.Unlock()
	unloaded := make([]Passenger, 0)
	if count >= len(f.Waitlist) {
		unloaded = append(unloaded, f.Waitlist...)
		f.Waitlist = f.Waitlist[:0]
	} else {
		unloaded = append(unloaded, f.Waitlist[:count]...)
		left := f.Waitlist[:0]
		left = append(left, f.Waitlist[count:]...)
		f.Waitlist = left
		log.Info(fmt.Sprintf("%d passengers are left on floor #%d", len(left), f.Number))
		f.RequestLift()
	}
	return unloaded
}

type RequestManager struct {
	m sync.Mutex
	backlog []int
}

func NewRequestManager(floorsCount int) *RequestManager {
	return &RequestManager{backlog: make([]int, 0, floorsCount)}
}

func (r *RequestManager) Listen(ctx context.Context, floors map[int]*Floor) {
	cases := make([]reflect.SelectCase, 0, len(floors) + 1)
	for i := 0; i < len(floors); i++ {
		cases = append(cases, reflect.SelectCase{
			Dir: reflect.SelectRecv,
			Chan: reflect.ValueOf(floors[i].button),
		})
	}
	cases = append(cases, reflect.SelectCase{
		Dir: reflect.SelectRecv,
		Chan: reflect.ValueOf(ctx.Done()),
	})
	for {
		floorNumber, _, _ := reflect.Select(cases)
		if floorNumber == len(floors) {
			log.Info("Shutting down request manager")
			return
		}
		r.m.Lock()
		if slices.Index(r.backlog, floorNumber) == -1 {
			r.backlog = append(r.backlog, floorNumber)
		}
		r.m.Unlock()
	}
}

func (r *RequestManager) ProcessRequests(ctx context.Context, liftFinder func (floorNumber int) bool) {
	ticker := time.NewTicker(tickDuration / 2)
	for {
		select {
		case <-ctx.Done():
			log.Info("Shutting down lift controller")
			return
		case <-ticker.C:
			r.m.Lock()
			n := r.backlog[:0]
			for _, floorNumber := range r.backlog {
				if ok := liftFinder(floorNumber); ok {
					continue
				}
				n = append(n, floorNumber)
			}
			r.backlog = n
			log.Info(fmt.Sprintf("Pending requests: %v", r.backlog))
			r.m.Unlock()
		}
	}
}

type Building struct {
	Lifts []*Lift
	Floors map[int]*Floor
}

func NewBuilding(floorsCount int) *Building {
	floors := make(map[int]*Floor, floorsCount)
	for i := 0; i < floorsCount; i++ {
		floors[i] = NewFloor(i)
	}
	lifts := []*Lift{
		&Lift{ID: 1, Passengers: make([]Passenger, 0, 5)},
		&Lift{ID: 2, Passengers: make([]Passenger, 0, 5)},
		&Lift{ID: 3, Passengers: make([]Passenger, 0, 5)},
	}
	return &Building{Floors: floors, Lifts: lifts}
}

func SpawnPassengers(ctx context.Context, floors map[int]*Floor, maxPassengers int) {
	ticker := time.NewTicker(2*tickDuration)
	for i := 0; i < maxPassengers; i++ {
		select {
		case <-ctx.Done():
			log.Info("Shutting down passenger generation")
			return
		case <-ticker.C:
			spawnFloorNumber := rand.Intn(len(floors))
			targetFloor := rand.Intn(len(floors))
			for {
				if spawnFloorNumber != targetFloor {
					break
				}
				targetFloor = rand.Intn(len(floors))
			}

			p := Passenger{
				StartingFloor: spawnFloorNumber,
				TargetFloor: targetFloor,
				Born: time.Now(),
			}
			log.Info(fmt.Sprintf("Spawning passenger %s", p.String()))
			floors[spawnFloorNumber].AddPassenger(p)
		}
	}
}

func FindLift(floorNumber int, lifts []*Lift) bool {
	for _, l := range lifts {
		if l.status != IDLE {
			continue
		}
		if ok := l.Call(floorNumber); ok {
			return true
		}
	}
	for _, l := range lifts {
		if l.status == IDLE {
			continue
		}
		if ok := l.Call(floorNumber); ok {
			return true
		}
	}
	return false
}

type Stats struct {
	Delivered int
	Waiting int
	Moving int
}

func (s *Stats) Collect(ctx context.Context, floors map[int]*Floor, lifts []*Lift) {
	ticker := time.NewTicker(tickDuration)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Waiting = 0
			s.Delivered = 0
			for _, f := range floors {
				s.Waiting += len(f.Waitlist)
				s.Delivered += len(f.Delivered)
			}

			s.Moving = 0
			for _, l := range lifts {
				s.Moving += len(l.Passengers)
			}
			log.Info(fmt.Sprintf("%+v", s))
		}
	}
}

func (s *Stats) PrintFinalStats(b *Building) {
	totalWaitTime := int64(0)
	totalMovingTime := int64(0)
	totalFloorsMoved := 0
	for _, f := range b.Floors {
		for _, p := range f.Delivered {
			totalWaitTime += p.Pickedup.Sub(p.Born).Microseconds()
			totalMovingTime += p.Delivered.Sub(p.Pickedup).Microseconds()
			floorsMoved := p.TargetFloor - p.StartingFloor
			if floorsMoved < 0 { floorsMoved = -floorsMoved }
			totalFloorsMoved += floorsMoved
		}
	}

	totalPassengers := s.Delivered + s.Moving + s.Waiting
	log.Info(fmt.Sprintf(
		"Average passenger waited - %f ticks, was moving - %f ticks, moved - %f floors",
		float64(totalWaitTime) / float64(tickDuration.Microseconds()) / float64(totalPassengers),
		float64(totalMovingTime) / float64(tickDuration.Microseconds()) / float64(totalPassengers),
		float64(totalFloorsMoved) / float64(totalPassengers),
	))
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	floorsCount := 25
	maxPassengers := 1000

	b := NewBuilding(floorsCount)

	for _, lift := range b.Lifts {
		go lift.Run(ctx, b.Floors)
	}

	r := NewRequestManager(floorsCount)
	go r.Listen(ctx, b.Floors)
	go r.ProcessRequests(ctx, func (f int) bool { return FindLift(f, b.Lifts) })
	
	var stats Stats
	go stats.Collect(ctx, b.Floors, b.Lifts)

	go SpawnPassengers(ctx, b.Floors, maxPassengers)

	sigChannel := make(chan os.Signal, 1)
	signal.Notify(sigChannel, os.Interrupt, syscall.SIGTERM)

main_loop:
	for stats.Delivered != maxPassengers {
		select {
		case <-sigChannel:
			break main_loop
		default:
			time.Sleep(50*tickDuration)
		}
	}

	cancel()
	stats.PrintFinalStats(b)
}