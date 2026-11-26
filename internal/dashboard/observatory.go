// Package dashboard serves Lemmings' localhost-only Synthwave '84 live portal,
// animated terrain observatory, SSE stream, and interactive Error Forensics
// inspection and triage review interface.
package dashboard

import (
	"strings"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

const (
	// StageSize bounds how many lemmings are animated simultaneously on canvas.
	StageSize          = 160
	maxTrackedLemmings = 10_000
	lifeHistory        = 32
	graveyardSize      = 500
	frameEventCap      = 800
	feedSize           = 200
	maxTerrainTiles    = 256
)

var feedBudget = map[string]int{
	"fail": 8, "queue": 4, "admit": 4, "home": 3, "unborn": 2,
	"terrain": 6, "drop": 1, "swarm": 4, "capture": 10,
}

// Observatory maintains the live dashboard projection of the swarm: animated
// stage slots, tracked lemming histories, terrain tiles, and a rate-limited feed.
// Every handler is O(1) under one short mutex so EventBus emitters never stall.
type Observatory struct {
	mu    sync.Mutex
	start time.Time
	phase string

	lives     map[string]*LemmingLife
	graveyard []string
	graveNext int

	stage   [StageSize]string
	free    []int
	events  []stageEvent
	visits  map[int]*stageEvent
	skipped int64

	terrains []terrainState
	feed     []feedItem
	feedSeq  uint64
	budget   map[string]int
	budgetAt time.Time
}

// LemmingLife is one tracked lemming served by GET /api/lemming.
type LemmingLife struct {
	Number   int64       `json:"number"`
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Persona  string      `json:"persona"`
	Language string      `json:"language"`
	Role     string      `json:"role,omitempty"`
	PlanName string      `json:"plan_name,omitempty"`
	Terrain  int         `json:"terrain"`
	Pack     int         `json:"pack"`
	Born     float64     `json:"born"`
	Died     float64     `json:"died,omitempty"`
	State    string      `json:"state"`
	QueuePos int         `json:"queue_pos,omitempty"`
	Visits   int64       `json:"visits"`
	Failed   int64       `json:"failed"`
	Exit     string      `json:"exit,omitempty"`
	Slot     int         `json:"slot"`
	Current  string      `json:"current,omitempty"`
	History  []LifeVisit `json:"history"`
	Omitted  int64       `json:"omitted"`
}

// LifeVisit is one page visit inside LemmingLife.
type LifeVisit struct {
	Seq        int     `json:"n"`
	At         float64 `json:"at"`
	Method     string  `json:"m,omitempty"`
	Step       string  `json:"step,omitempty"`
	Path       string  `json:"p"`
	Status     int     `json:"s"`
	Millis     float64 `json:"ms"`
	TTFB       float64 `json:"ttfb"`
	Bytes      int64   `json:"b"`
	Title      string  `json:"title,omitempty"`
	Failed     bool    `json:"f,omitempty"`
	Cancelled  bool    `json:"c,omitempty"`
	Why        string  `json:"why,omitempty"`
	QueuedMS   float64 `json:"q,omitempty"`
	Hops       int     `json:"hops,omitempty"`
	Reused     bool    `json:"reused,omitempty"`
	ArtifactID string  `json:"artifact_id,omitempty"`
}

type stageEvent struct {
	Kind     string  `json:"k"`
	Slot     int     `json:"s"`
	ID       string  `json:"id,omitempty"`
	Number   int64   `json:"num,omitempty"`
	Name     string  `json:"n,omitempty"`
	Terrain  int     `json:"t,omitempty"`
	Count    int     `json:"c,omitempty"`
	Status   int     `json:"st,omitempty"`
	Failed   bool    `json:"f,omitempty"`
	Path     string  `json:"p,omitempty"`
	Millis   float64 `json:"ms,omitempty"`
	Pos      int     `json:"pos,omitempty"`
	Exit     string  `json:"x,omitempty"`
	Flawless bool    `json:"ok,omitempty"`
}

type stageSlot struct {
	Slot     int    `json:"s"`
	ID       string `json:"id"`
	Number   int64  `json:"num,omitempty"`
	Name     string `json:"n"`
	Terrain  int    `json:"t"`
	State    string `json:"state"`
	QueuePos int    `json:"pos,omitempty"`
}

type feedItem struct {
	Seq        uint64  `json:"seq"`
	T          float64 `json:"t"`
	Kind       string  `json:"k"`
	ID         string  `json:"id,omitempty"`
	Number     int64   `json:"num,omitempty"`
	Name       string  `json:"n,omitempty"`
	Terrain    int     `json:"tr"`
	Path       string  `json:"p,omitempty"`
	Status     int     `json:"st,omitempty"`
	Millis     float64 `json:"ms,omitempty"`
	Detail     string  `json:"x,omitempty"`
	ArtifactID string  `json:"artifact_id,omitempty"`
}

type terrainState struct {
	state  uint8 // 0 waiting, 1 online, 2 done
	alive  int64
	visits int64
	failed int64
}

type terrainTile struct {
	From   int   `json:"from"`
	To     int   `json:"to"`
	State  uint8 `json:"s"`
	Alive  int64 `json:"a"`
	Visits int64 `json:"v"`
	Failed int64 `json:"f"`
}

type frame struct {
	T       float64      `json:"t"`
	Phase   string       `json:"phase"`
	Events  []stageEvent `json:"ev,omitempty"`
	Skipped int64        `json:"skipped,omitempty"`
	Feed    []feedItem   `json:"feed,omitempty"`
	Stage   []stageSlot  `json:"stage,omitempty"`
}

// NewObservatory initializes an Observatory for terrains terrain groups.
func NewObservatory(terrains int64) *Observatory {
	o := &Observatory{
		start:    time.Now(),
		phase:    "ramping",
		lives:    make(map[string]*LemmingLife),
		visits:   make(map[int]*stageEvent),
		terrains: make([]terrainState, max(terrains, 0)),
		budget:   make(map[string]int),
	}
	for i := StageSize - 1; i >= 0; i-- {
		o.free = append(o.free, i)
	}
	return o
}

func (o *Observatory) markStarted(at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.start = at
}

func (o *Observatory) since(at time.Time) float64 {
	return at.Sub(o.start).Seconds()
}

func (o *Observatory) terrain(id int) *terrainState {
	if id < 0 || id >= len(o.terrains) {
		return nil
	}
	return &o.terrains[id]
}

// Handle processes a single EventBus event in O(1) time.
func (o *Observatory) Handle(e events.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.since(e.OccurredAt)

	switch e.Kind {
	case events.LemmingBorn:
		if ts := o.terrain(e.Terrain); ts != nil {
			ts.alive++
		}
		life := &LemmingLife{
			Number:   e.LemmingNumber,
			ID:       e.LemmingID,
			Name:     swarm.LemmingName(e.LemmingID),
			Role:     e.Role,
			PlanName: e.PlanName,
			Terrain:  e.Terrain,
			Pack:     e.Pack,
			Born:     t,
			State:    "walking",
			Slot:     -1,
		}
		if id, ok := e.Payload.(*swarm.Identity); ok && id != nil {
			life.Number = id.Number
			life.Name = id.Name
			life.Persona = swarm.PersonaOf(id.UserAgent)
			life.Language = id.Language
			if id.Role != "" {
				life.Role = id.Role
			}
			if id.PlanName != "" {
				life.PlanName = id.PlanName
			}
		}
		if len(o.lives) < maxTrackedLemmings {
			o.lives[e.LemmingID] = life
		}
		if n := len(o.free); n > 0 {
			slot := o.free[n-1]
			o.free = o.free[:n-1]
			o.stage[slot] = e.LemmingID
			life.Slot = slot
			o.push(stageEvent{Kind: "b", Slot: slot, ID: e.LemmingID, Number: life.Number, Name: life.Name, Terrain: e.Terrain})
		}

	case events.VisitComplete, events.VisitError:
		v, ok := e.Payload.(*swarm.Visit)
		if !ok || v == nil {
			return
		}
		failed := swarm.FailedVisit(v)
		if ts := o.terrain(e.Terrain); ts != nil {
			ts.visits++
			if failed {
				ts.failed++
			}
		}
		life := o.lives[e.LemmingID]
		if life != nil {
			life.record(v, t)
		}
		if failed {
			name := swarm.LemmingName(e.LemmingID)
			if life != nil {
				name = life.Name
			}
			o.publish(feedItem{
				T: t, Kind: "fail", ID: e.LemmingID, Number: e.LemmingNumber, Name: name, Terrain: e.Terrain,
				Path: discovery.PathOf(v.URL), Status: v.StatusCode, Millis: report.Millis(v.EffectiveLatency()), Detail: shortWhy(v),
			})
		}
		if life != nil && life.Slot >= 0 {
			pv := o.visits[life.Slot]
			if pv == nil {
				pv = &stageEvent{Kind: "v", Slot: life.Slot}
				o.visits[life.Slot] = pv
			}
			pv.Count++
			if failed || !pv.Failed {
				pv.Status, pv.Failed, pv.Path, pv.Millis = v.StatusCode, failed, discovery.PathOf(v.URL), report.Millis(v.EffectiveLatency())
			}
		}

	case events.ArtifactCaptured:
		if sum, ok := e.Payload.(forensics.Summary); ok {
			if life := o.lives[sum.LemmingID]; life != nil {
				for i := len(life.History) - 1; i >= 0; i-- {
					if life.History[i].Seq == sum.VisitSeq {
						life.History[i].ArtifactID = sum.ID
						break
					}
				}
			}
			o.publish(feedItem{
				T:          t,
				Kind:       "capture",
				ID:         sum.LemmingID,
				Number:     sum.LemmingNumber,
				Name:       sum.LemmingName,
				Terrain:    e.Terrain,
				Path:       sum.Path,
				Status:     sum.StatusCode,
				Detail:     sum.TriggerReason,
				ArtifactID: sum.ID,
			})
		}

	case events.WaitingRoomQueued:
		life := o.lives[e.LemmingID]
		first := life == nil || life.State != "queued"
		if life != nil {
			life.State, life.QueuePos = "queued", e.Position
			if life.Slot >= 0 {
				o.flushVisit(life.Slot)
				o.push(stageEvent{Kind: "q", Slot: life.Slot, Pos: e.Position})
			}
		}
		if first {
			o.publish(feedItem{
				T: t, Kind: "queue", ID: e.LemmingID, Number: e.LemmingNumber,
				Name: swarm.LemmingName(e.LemmingID), Terrain: e.Terrain,
				Path: discovery.PathOf(e.URL), Detail: report.FormatInt(int64(e.Position)),
			})
		}

	case events.WaitingRoom:
		if life := o.lives[e.LemmingID]; life != nil {
			life.State, life.QueuePos = "walking", 0
			if life.Slot >= 0 {
				o.push(stageEvent{Kind: "a", Slot: life.Slot})
			}
		}
		o.publish(feedItem{
			T: t, Kind: "admit", ID: e.LemmingID, Number: e.LemmingNumber,
			Name: swarm.LemmingName(e.LemmingID), Terrain: e.Terrain,
			Path: discovery.PathOf(e.URL), Millis: report.Millis(e.Duration),
		})

	case events.LemmingDied:
		if ts := o.terrain(e.Terrain); ts != nil {
			ts.alive--
		}
		var flawless bool
		exit := ""
		var totalVisits int64
		if ll, ok := e.Payload.(*swarm.LifeLog); ok && ll != nil {
			flawless = ll.FailedVisits == 0 && ll.Error == nil
			exit = ll.ExitReason
			totalVisits = ll.TotalVisits
		}
		life := o.lives[e.LemmingID]
		if life != nil {
			life.Died, life.Exit, life.QueuePos = t, exit, 0
			life.State = "gone"
			if flawless {
				life.State = "home"
			}
			if life.Slot >= 0 {
				o.flushVisit(life.Slot)
				o.push(stageEvent{Kind: "d", Slot: life.Slot, Exit: exit, Flawless: flawless})
				o.stage[life.Slot] = ""
				o.free = append(o.free, life.Slot)
				life.Slot = -1
			}
			o.bury(life.ID)
		}
		if flawless && totalVisits > 0 {
			o.publish(feedItem{
				T: t, Kind: "home", ID: e.LemmingID, Number: e.LemmingNumber,
				Name: swarm.LemmingName(e.LemmingID), Terrain: e.Terrain,
				Detail: report.FormatInt(totalVisits),
			})
		}

	case events.LemmingFailed:
		o.publish(feedItem{T: t, Kind: "unborn", Terrain: e.Terrain, Detail: errText(e.Err)})

	case events.TerrainOnline, events.TerrainDone:
		state, detail := uint8(1), "online"
		if e.Kind == events.TerrainDone {
			state, detail = 2, "done"
		}
		if ts := o.terrain(e.Terrain); ts != nil {
			ts.state = state
		}
		o.publish(feedItem{T: t, Kind: "terrain", Terrain: e.Terrain, Detail: detail})

	case events.LogDropped:
		o.publish(feedItem{T: t, Kind: "drop", Detail: "life records dropped: collector is behind"})

	case events.SwarmStarted:
		o.phase = "ramping"
		o.publish(feedItem{T: t, Kind: "swarm", Detail: "the swarm is moving"})

	case events.SwarmDone:
		if o.phase != "reviewing" {
			o.phase = "done"
		}
		o.publish(feedItem{T: t, Kind: "swarm", Detail: "all lemmings accounted for"})
	}
}

func shortWhy(v *swarm.Visit) string {
	why := swarm.WhyFailed(v)
	if name, _, ok := strings.Cut(why, ":"); ok {
		why = name
	}
	if why == "status" || strings.HasPrefix(why, "status ") {
		return ""
	}
	return why
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return discovery.Clip(err.Error(), 160)
}

func (l *LemmingLife) record(v *swarm.Visit, t float64) {
	l.Visits++
	if swarm.FailedVisit(v) {
		l.Failed++
	}
	l.Current = discovery.PathOf(v.URL)
	lv := LifeVisit{
		Seq: v.Sequence, At: t, Method: v.Method, Step: v.Step, Path: discovery.PathOf(v.URL), Status: v.StatusCode,
		Millis: report.Millis(v.EffectiveLatency()), TTFB: report.Millis(v.Timing.TTFB), Bytes: v.BytesIn,
		Title: v.Page.Title, Failed: swarm.FailedVisit(v), Cancelled: v.Cancelled, Why: swarm.WhyFailed(v),
		QueuedMS: report.Millis(v.WaitingRoom.Duration), Hops: max(len(v.Hops)-1, 0), Reused: v.Timing.Reused,
	}
	if len(l.History) >= lifeHistory {
		copy(l.History, l.History[1:])
		l.History = l.History[:lifeHistory-1]
		l.Omitted++
	}
	l.History = append(l.History, lv)
}

func (o *Observatory) push(ev stageEvent) {
	if len(o.events) >= frameEventCap {
		o.skipped++
		return
	}
	o.events = append(o.events, ev)
}

func (o *Observatory) flushVisit(slot int) {
	if pv := o.visits[slot]; pv != nil {
		o.push(*pv)
		delete(o.visits, slot)
	}
}

func (o *Observatory) bury(id string) {
	if len(o.graveyard) < graveyardSize {
		o.graveyard = append(o.graveyard, id)
		return
	}
	delete(o.lives, o.graveyard[o.graveNext])
	o.graveyard[o.graveNext] = id
	o.graveNext = (o.graveNext + 1) % graveyardSize
}

func (o *Observatory) publish(item feedItem) {
	now := time.Now()
	if now.Sub(o.budgetAt) >= time.Second {
		clear(o.budget)
		o.budgetAt = now
	}
	if o.budget[item.Kind] >= feedBudget[item.Kind] {
		return
	}
	o.budget[item.Kind]++
	o.feedSeq++
	item.Seq = o.feedSeq
	if len(o.feed) < feedSize {
		o.feed = append(o.feed, item)
		return
	}
	o.feed[int(item.Seq-1)%feedSize] = item
}

func (o *Observatory) feedSince(seq uint64) []feedItem {
	var out []feedItem
	if len(o.feed) == 0 {
		return nil
	}
	oldest := o.feedSeq - uint64(len(o.feed)) + 1
	for s := max(seq+1, oldest); s <= o.feedSeq; s++ {
		out = append(out, o.feed[int(s-1)%feedSize])
	}
	return out
}

func (o *Observatory) frame(lastFeed uint64, withStage bool) (frame, uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()

	f := frame{T: o.since(time.Now()), Phase: o.phase, Skipped: o.skipped}
	f.Events = o.events
	o.events = nil
	o.skipped = 0
	for slot := range o.visits {
		f.Events = append(f.Events, *o.visits[slot])
	}
	clear(o.visits)
	f.Feed = o.feedSince(lastFeed)
	if withStage {
		f.Stage = o.stageLocked()
	}
	return f, o.feedSeq
}

func (o *Observatory) stageLocked() []stageSlot {
	out := make([]stageSlot, 0, StageSize)
	for slot, id := range o.stage {
		if id == "" {
			continue
		}
		s := stageSlot{Slot: slot, ID: id, Name: swarm.LemmingName(id), State: "walking"}
		if l := o.lives[id]; l != nil {
			s.Number, s.Terrain, s.State, s.QueuePos = l.Number, l.Terrain, l.State, l.QueuePos
		}
		out = append(out, s)
	}
	return out
}

// SetPhase updates the swarm phase displayed in the portal header.
func (o *Observatory) SetPhase(phase string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.phase != "done" || phase == "reviewing" {
		o.phase = phase
	}
}

// Life returns a defensive copy of one tracked lemming's life.
func (o *Observatory) Life(id string) (LemmingLife, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	l := o.lives[id]
	if l == nil {
		return LemmingLife{}, false
	}
	c := *l
	c.History = append([]LifeVisit(nil), l.History...)
	return c, true
}

func (o *Observatory) terrainTiles() []terrainTile {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(o.terrains)
	if n == 0 {
		return nil
	}
	tiles := min(n, maxTerrainTiles)
	out := make([]terrainTile, tiles)
	for i := range out {
		from, to := i*n/tiles, (i+1)*n/tiles-1
		tile := terrainTile{From: from, To: to, State: 2}
		anyWaiting := false
		for t := from; t <= to; t++ {
			ts := o.terrains[t]
			tile.Alive += ts.alive
			tile.Visits += ts.visits
			tile.Failed += ts.failed
			switch ts.state {
			case 1:
				tile.State = 1
			case 0:
				anyWaiting = true
			}
		}
		if tile.State != 1 && anyWaiting {
			tile.State = 0
		}
		out[i] = tile
	}
	return out
}

func (o *Observatory) snapshot() (float64, string, []stageSlot, []feedItem, uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.since(time.Now()), o.phase, o.stageLocked(), o.feedSince(0), o.feedSeq
}
