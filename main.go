package main

import (
	// "errors"
	"context"
	"debug/buildinfo"
	"fmt"
	"go/build"
	"math/rand"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	// "log/slog"
)

// logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

type liftStatus int

const (
	IDLE liftStatus = 0
	DOWN liftStatus = 1
	UP liftStatus = 2
	DefaultLiftCapacity = 5
)

func popFirst(slice []int) []int {
	n := slice[:0]
	return append(n, slice[1:]...)
}

type Passenger struct {
	StartingFloor int
	TargetFloor int
}

func (p Passenger) String() string {
	return fmt.Sprintf("P[%d->%d]", p.StartingFloor, p.TargetFloor)
}

type Lift struct {
	ID int
	// add lock for passengers?
	Passengers []Passenger
	CurrentFloor int

	backlog []int  // floors to be visited after finishing current run
	destinations []int  // floors to be visited during current run
	status liftStatus
	m sync.Mutex  // lock to protect backlog, destinations and status
}

func (l *Lift) String() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Lift #%d ", l.ID))

	if l.status == UP {
		b.WriteString("UP ")
	} else if l.status == DOWN {
		b.WriteString("DOWN ")
	} else if l.status == IDLE {
		b.WriteString("IDLE ")
	}

	b.WriteString("[")
	for _, p := range l.Passengers {
		b.WriteString(p.String())
	}
	b.WriteString("] ")

	b.WriteString(fmt.Sprintf("%v %v", l.destinations, l.backlog))
	b.WriteString("\n")
	return b.String()
}

func (l *Lift) Call(floor *Floor) (ok bool) {
	if floor.Number == l.CurrentFloor {
		// THIS IS BAD DESIGN
		l.LoadUnload(floor)
		return true
	}

	l.m.Lock()
	defer l.m.Unlock()

	if l.status == DOWN && floor.Number > l.CurrentFloor {
		return false
	}
	if l.status == UP && floor.Number < l.CurrentFloor {
		return false
	}

	l.AddDestination(floor.Number)
	return true
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
			l.status = DOWN
			fmt.Printf("Lift #%d starts going down\n", l.ID)
		} else if l.CurrentFloor < floorNumber{
			l.status = UP
			fmt.Printf("Lift #%d starts going up\n", l.ID)
		}
	}
}

func (l *Lift) PressButton(floorNumber int) {
	if floorNumber == l.CurrentFloor {
		return
	}

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

	if l.status == IDLE && len(l.destinations) != 0 {
		l.AddToBacklog(floorNumber)
		return
	}

	l.AddDestination(floorNumber)
}

func (l *Lift) UpdateDestinations() {
	l.m.Lock()
	defer l.m.Unlock()

	if l.status == DOWN {
		l.destinations = l.destinations[:len(l.destinations) - 1]
	} else if l.status == UP {
		l.destinations = popFirst(l.destinations)
	}

	if len(l.destinations) != 0 {
		return
	}

	if len(l.backlog) == 0 {
		l.status = IDLE
		fmt.Printf("Lift #%d becomes idle\n", l.ID)
		return
	}

	// change direction to opposite
	l.destinations = append(l.destinations, l.backlog...)
	l.backlog = l.backlog[:0]
	if l.status == UP {
		l.status = DOWN
		fmt.Printf("Lift #%d starts going down\n", l.ID)
	} else if l.status == DOWN {
		l.status = UP
		fmt.Printf("Lift #%d starts going up\n", l.ID)
	}
}

func (l *Lift) CurrentDestination() int {
	l.m.Lock()
	defer l.m.Unlock()

	if l.status == DOWN {
		return l.destinations[len(l.destinations) - 1]
	} else if l.status == UP {
		return l.destinations[0]
	}
	return -1
}

func (l *Lift) LoadUnload(floor *Floor) {
	fmt.Printf("Lift #%d opens doors on floor #%d\n", l.ID, floor.Number)
	staying := l.Passengers[:0]
	for _, p := range l.Passengers {
		if p.TargetFloor != floor.Number {
			staying = append(staying, p)
		}
	}
	fmt.Printf("Lift #%d unloads %d passengers\n", l.ID, len(l.Passengers) - len(staying))
	l.Passengers = staying
	// TODO check capacity
	incoming := floor.Unload()
	l.Passengers = append(l.Passengers, incoming...)

	fmt.Printf("Lift #%d loads %d passengers\n", l.ID, len(incoming))
	fmt.Printf(l.String())
	for _, p := range incoming {
		l.PressButton(p.TargetFloor)
	}
}

// is this neccessary?
type Floor struct {
	Number int
	Waitlist []Passenger
	m sync.Mutex
	LiftRequests chan int
}

func (f *Floor) AddPassenger(p Passenger) {
	f.m.Lock()
	defer f.m.Unlock()
	f.Waitlist = append(f.Waitlist, p)
	f.LiftRequests <- f.Number
}

func (f *Floor) Unload() []Passenger {
	f.m.Lock()
	defer f.m.Unlock()
	res := make([]Passenger, 0, len(f.Waitlist))
	res = append(res, f.Waitlist...)
	f.Waitlist = f.Waitlist[:0]
	return res
}

type Building struct {
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
			LiftRequests: liftRequests,
		}
	}

	lifts := []*Lift{
		&Lift{ID: 1, Passengers: make([]Passenger, 0, 100)},
		// &Lift{ID: 2, Passengers: make([]Passenger, 0, 100)},
	}

	b := &Building{
		Floors: floors,
		Lifts: lifts,
		LiftRequests: liftRequests,
		Backlog: make([]int, 0, floorsCount),
	}
	return b
}

func (b *Building) ListenCalls(ctx context.Context) {
	ticker := time.NewTicker(400 * time.Millisecond)
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("Shutting down lift controller\n")
			return
		case <-ticker.C:
			if len(b.Backlog) > 0 {
				fmt.Printf("Controller backlog: %v\n", b.Backlog)
				newBacklog := b.Backlog[:0]
backlog_loop:
				for _, num := range b.Backlog {
					for _, l := range b.Lifts {
						if ok := l.Call(b.Floors[num]); ok == true {
							fmt.Printf("Successfully called lift #%d[at #%d] to floor #%d\n", l.ID, l.CurrentFloor, num)
							continue backlog_loop
						}
					}
					newBacklog = append(newBacklog, num)
				}
				b.Backlog = newBacklog
			}
		case floorNumber := <-b.LiftRequests:
			fmt.Printf("Request received: passenger is waiting on floor #%d\n", floorNumber)
			if slices.Index(b.Backlog, floorNumber) == -1 {
				b.Backlog = append(b.Backlog, floorNumber)
			}
		}
	}
}

func (b *Building) RunLifts(ctx context.Context) {
	ticker := time.NewTicker(1000 * time.Millisecond)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, l := range b.Lifts {
				fmt.Printf(l.String())
				if l.status == DOWN {
					l.CurrentFloor--
					fmt.Printf("Lift #%d arrives at floor #%d\n", l.ID, l.CurrentFloor)
				} else if l.status == UP {
					l.CurrentFloor++
					fmt.Printf("Lift #%d arrives at floor #%d\n", l.ID, l.CurrentFloor)
				}

				if l.CurrentFloor == l.CurrentDestination() {
					l.UpdateDestinations()
					l.LoadUnload(b.Floors[l.CurrentFloor])
				}
			}
		}
	}
}


func SpawnPassengers(ctx context.Context, b *Building) {
	ticker := time.NewTicker(2*time.Second)
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("Shutting down passenger generation\n")
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

			p := Passenger{spawnFloorNumber, targetFloor}
			fmt.Printf("Spawning passenger %s\n", p.String())
			b.Floors[spawnFloorNumber].AddPassenger(p)
		}
	}
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	sigChannel := make(chan os.Signal, 1)
	signal.Notify(sigChannel, os.Interrupt, syscall.SIGTERM)

	floorsCount := 10
	b := NewBuilding(ctx, floorsCount)
	go b.ListenCalls(ctx)
	go b.RunLifts(ctx)
	go SpawnPassengers(ctx, b)

	<-sigChannel
	cancel()
	time.Sleep(100 * time.Microsecond)
	os.Exit(0)
}