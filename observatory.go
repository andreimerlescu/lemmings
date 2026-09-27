package main

import (
	"strings"
	"sync"
	"time"
)

const (
	// stageSize is how many lemmings the dashboard animates at once. Every
	// lemming on stage has its whole life animated; the rest of the swarm
	// is shown through counters, charts and the terrain map. The stage
	// keeps the browser's work constant no matter how large the swarm.
	stageSize = 160

	// maxTrackedLemmings bounds the lemmings whose recent life can be
	// inspected. Beyond it new lemmings are counted but not tracked.
	maxTrackedLemmings = 10_000

	// lifeHistory is how many recent visits are kept per tracked lemming.
	lifeHistory = 32

	// graveyardSize is how many dead lemmings stay inspectable.
	graveyardSize = 500

	// frameEventCap bounds the stage events carried by one frame. Anything
	// beyond is counted as skipped and healed by the next stage snapshot.
	frameEventCap = 800

	// feedSize is how many feed items are kept for late joiners.
	feedSize = 200

	// maxTerrainTiles bounds the terrain map. Larger swarms group terrains
	// into this many tiles.
	maxTerrainTiles = 256
)

// feedBudget is how many feed items of each kind may be published per
// second, so a storm of failures stays readable.
var feedBudget = map[string]int{
	"fail": 8, "queue": 4, "admit": 4, "home": 3, "unborn": 2,
	"terrain": 6, "drop": 1, "swarm": 4,
}

// Observatory keeps the live picture of the swarm for the dashboard: who
// is on stage, each tracked lemming's recent life, per-terrain activity
// and a feed of notable happenings. It is an EventBus subscriber, so every
// handler is O(1) under one short lock.
type Observatory struct {
	mu    sync.Mutex
	start time.Time
	phase string

	lives     map[string]*lemmingLife
	graveyard []string // ring of dead lemming IDs, oldest evicted first
	graveNext int

	stage   [stageSize]string // lemming ID per slot, "" when free
	free    []int             // free stage slots, used as a stack
	events  []stageEvent      // ordered born/queued/admitted/died events
	visits  map[int]*stageEvent
	skipped int64

	terrains []terrainState
	feed     []feedItem // ring buffer of the last feedSize items
	feedSeq  uint64
	budget   map[string]int
	budgetAt time.Time
}

// lemmingLife is one tracked lemming.
type lemmingLife struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Persona  string      `json:"persona"`
	Language string      `json:"language"`
	Terrain  int         `json:"terrain"`
	Pack     int         `json:"pack"`
	Born     float64     `json:"born"`           // seconds since start
	Died     float64     `json:"died,omitempty"` // seconds since start
	State    string      `json:"state"`          // walking, queued, home, gone
	QueuePos int         `json:"queue_pos,omitempty"`
	Visits   int64       `json:"visits"`
	Failed   int64       `json:"failed"`
	Exit     string      `json:"exit,omitempty"`
	Slot     int         `json:"slot"` // -1 when not on stage
	Current  string      `json:"current,omitempty"`
	History  []lifeVisit `json:"history"`
	Omitted  int64       `json:"omitted"`
}

// lifeVisit is one visit as the inspector shows it.
type lifeVisit struct {
	Seq       int     `json:"n"`
	At        float64 `json:"at"` // seconds since start
	Step      string  `json:"step,omitempty"`
	Path      string  `json:"p"`
	Status    int     `json:"s"`
	Millis    float64 `json:"ms"`
	TTFB      float64 `json:"ttfb"`
	Bytes     int64   `json:"b"`
	Title     string  `json:"title,omitempty"`
	Failed    bool    `json:"f,omitempty"`
	Cancelled bool    `json:"c,omitempty"`
	Why       string  `json:"why,omitempty"`
	QueuedMS  float64 `json:"q,omitempty"`
	Hops      int     `json:"hops,omitempty"`
	Reused    bool    `json:"reused,omitempty"`
}

// stageEvent is one thing that happened to a lemming on stage. Kinds:
// b born, v visited, q queued, a admitted, d died.
type stageEvent struct {
	Kind     string  `json:"k"`
	Slot     int     `json:"s"`
	ID       string  `json:"id,omitempty"`
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

// stageSlot is one occupied slot in a stage snapshot.
type stageSlot struct {
	Slot     int    `json:"s"`
	ID       string `json:"id"`
	Name     string `json:"n"`
	Terrain  int    `json:"t"`
	State    string `json:"state"`
	QueuePos int    `json:"pos,omitempty"`
}

// feedItem is one line in the dashboard's feed.
type feedItem struct {
	Seq     uint64  `json:"seq"`
	T       float64 `json:"t"`
	Kind    string  `json:"k"`
	ID      string  `json:"id,omitempty"`
	Name    string  `json:"n,omitempty"`
	Terrain int     `json:"tr"`
	Path    string  `json:"p,omitempty"`
	Status  int     `json:"st,omitempty"`
	Millis  float64 `json:"ms,omitempty"`
	Detail  string  `json:"x,omitempty"`
}

// terrainState is the live state of one terrain.
type terrainState struct {
	state  uint8 // 0 waiting, 1 online, 2 done
	alive  int64
	visits int64
	failed int64
}

// terrainTile is one tile of the terrain map, covering one or more terrains.
type terrainTile struct {
	From   int   `json:"from"`
	To     int   `json:"to"`
	State  uint8 `json:"s"`
	Alive  int64 `json:"a"`
	Visits int64 `json:"v"`
	Failed int64 `json:"f"`
}

// frame is what the browser receives several times a second.
type frame struct {
	T       float64      `json:"t"`
	Phase   string       `json:"phase"`
	Events  []stageEvent `json:"ev,omitempty"`
	Skipped int64        `json:"skipped,omitempty"`
	Feed    []feedItem   `json:"feed,omitempty"`
	Stage   []stageSlot  `json:"stage,omitempty"`
}

// NewObservatory returns an observatory for a swarm of the given terrains.
func NewObservatory(terrains int64) *Observatory {
	o := &Observatory{
		start:    time.Now(),
		phase:    "ramping",
		lives:    make(map[string]*lemmingLife),
		visits:   make(map[int]*stageEvent),
		terrains: make([]terrainState, max(terrains, 0)),
		budget:   make(map[string]int),
	}
	for i := stageSize - 1; i >= 0; i-- {
		o.free = append(o.free, i)
	}
	return o
}

// markStarted resets the clock the dashboard measures time against.
func (o *Observatory) markStarted(at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.start = at
}

// since returns seconds since start. o.mu must be held.
func (o *Observatory) since(at time.Time) float64 {
	return at.Sub(o.start).Seconds()
}

// terrain returns the state for id, or nil when out of range.
func (o *Observatory) terrain(id int) *terrainState {
	if id < 0 || id >= len(o.terrains) {
		return nil
	}
	return &o.terrains[id]
}

// handle is the observatory's EventBus subscriber.
func (o *Observatory) handle(e Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.since(e.OccurredAt)

	switch e.Kind {
	case EventLemmingBorn:
		if ts := o.terrain(e.Terrain); ts != nil {
			ts.alive++
		}
		life := &lemmingLife{
			ID: e.LemmingID, Name: lemmingName(e.LemmingID), Terrain: e.Terrain, Pack: e.Pack,
			Born: t, State: "walking", Slot: -1,
		}
		if id := e.Identity; id != nil {
			life.Name, life.Persona, life.Language = id.Name, personaOf(id.UserAgent), id.Language
		}
		if len(o.lives) < maxTrackedLemmings {
			o.lives[e.LemmingID] = life
		}
		if n := len(o.free); n > 0 {
			slot := o.free[n-1]
			o.free = o.free[:n-1]
			o.stage[slot] = e.LemmingID
			life.Slot = slot
			o.push(stageEvent{Kind: "b", Slot: slot, ID: e.LemmingID, Name: life.Name, Terrain: e.Terrain})
		}

	case EventVisitComplete, EventVisitError:
		v := e.Visit
		if v == nil {
			return
		}
		failed := failedVisit(v)
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
			name := lemmingName(e.LemmingID)
			if life != nil {
				name = life.Name
			}
			o.publish(feedItem{
				T: t, Kind: "fail", ID: e.LemmingID, Name: name, Terrain: e.Terrain,
				Path: pathOf(v.URL), Status: v.StatusCode, Millis: millis(v.latency()), Detail: shortWhy(v),
			})
		}
		if life != nil && life.Slot >= 0 {
			pv := o.visits[life.Slot]
			if pv == nil {
				pv = &stageEvent{Kind: "v", Slot: life.Slot}
				o.visits[life.Slot] = pv
			}
			pv.Count++
			// Keep the most telling visit of the frame: any failure beats a success.
			if failed || !pv.Failed {
				pv.Status, pv.Failed, pv.Path, pv.Millis = v.StatusCode, failed, pathOf(v.URL), millis(v.latency())
			}
		}

	case EventWaitingRoomQueued:
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
			o.publish(feedItem{T: t, Kind: "queue", ID: e.LemmingID, Name: lemmingName(e.LemmingID),
				Terrain: e.Terrain, Path: pathOf(e.URL), Detail: formatInt(int64(e.Position))})
		}

	case EventWaitingRoom:
		if life := o.lives[e.LemmingID]; life != nil {
			life.State, life.QueuePos = "walking", 0
			if life.Slot >= 0 {
				o.push(stageEvent{Kind: "a", Slot: life.Slot})
			}
		}
		o.publish(feedItem{T: t, Kind: "admit", ID: e.LemmingID, Name: lemmingName(e.LemmingID),
			Terrain: e.Terrain, Path: pathOf(e.URL), Millis: millis(e.Duration)})

	case EventLemmingDied:
		if ts := o.terrain(e.Terrain); ts != nil {
			ts.alive--
		}
		var flawless bool
		exit := ""
		if e.Life != nil {
			flawless = e.Life.FailedVisits == 0 && e.Life.Error == nil
			exit = e.Life.ExitReason
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
		if flawless && e.Life != nil {
			o.publish(feedItem{T: t, Kind: "home", ID: e.LemmingID, Name: lemmingName(e.LemmingID),
				Terrain: e.Terrain, Detail: formatInt(e.Life.TotalVisits)})
		}

	case EventLemmingFailed:
		o.publish(feedItem{T: t, Kind: "unborn", Terrain: e.Terrain, Detail: errText(e.Err)})

	case EventTerrainOnline, EventTerrainDone:
		state, detail := uint8(1), "online"
		if e.Kind == EventTerrainDone {
			state, detail = 2, "done"
		}
		if ts := o.terrain(e.Terrain); ts != nil {
			ts.state = state
		}
		o.publish(feedItem{T: t, Kind: "terrain", Terrain: e.Terrain, Detail: detail})

	case EventLogDropped:
		o.publish(feedItem{T: t, Kind: "drop", Detail: "life records dropped: collector is behind"})

	case EventSwarmStarted:
		o.phase = "ramping"
		o.publish(feedItem{T: t, Kind: "swarm", Detail: "the swarm is moving"})

	case EventSwarmDone:
		o.phase = "done"
		o.publish(feedItem{T: t, Kind: "swarm", Detail: "all lemmings accounted for"})
	}
}

// shortWhy is whyFailed without detail the feed already shows: the check
// name alone, and nothing for a bad status (the status is shown).
func shortWhy(v *Visit) string {
	why := whyFailed(v)
	if name, _, ok := strings.Cut(why, ":"); ok {
		why = name
	}
	if why == "status" || strings.HasPrefix(why, "status ") {
		return ""
	}
	return why
}

// errText renders an optional error.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return clip(err.Error(), 160)
}

// record appends a visit to the lemming's history.
func (l *lemmingLife) record(v *Visit, t float64) {
	l.Visits++
	if failedVisit(v) {
		l.Failed++
	}
	l.Current = pathOf(v.URL)
	lv := lifeVisit{
		Seq: v.Sequence, At: t, Step: v.Step, Path: pathOf(v.URL), Status: v.StatusCode,
		Millis: millis(v.latency()), TTFB: millis(v.Timing.TTFB), Bytes: v.BytesIn,
		Title: v.Page.Title, Failed: failedVisit(v), Cancelled: v.Cancelled, Why: whyFailed(v),
		QueuedMS: millis(v.WaitingRoom.Duration), Hops: max(len(v.Hops)-1, 0), Reused: v.Timing.Reused,
	}
	if len(l.History) >= lifeHistory {
		copy(l.History, l.History[1:])
		l.History = l.History[:lifeHistory-1]
		l.Omitted++
	}
	l.History = append(l.History, lv)
}

// push appends an ordered stage event. o.mu must be held.
func (o *Observatory) push(ev stageEvent) {
	if len(o.events) >= frameEventCap {
		o.skipped++
		return
	}
	o.events = append(o.events, ev)
}

// flushVisit moves a slot's collapsed visits into the ordered stream so
// they are attributed to the current occupant. o.mu must be held.
func (o *Observatory) flushVisit(slot int) {
	if pv := o.visits[slot]; pv != nil {
		o.push(*pv)
		delete(o.visits, slot)
	}
}

// bury moves a dead lemming to the graveyard, evicting the oldest grave.
// o.mu must be held.
func (o *Observatory) bury(id string) {
	if len(o.graveyard) < graveyardSize {
		o.graveyard = append(o.graveyard, id)
		return
	}
	delete(o.lives, o.graveyard[o.graveNext])
	o.graveyard[o.graveNext] = id
	o.graveNext = (o.graveNext + 1) % graveyardSize
}

// publish adds a feed item if its kind still has budget this second.
// o.mu must be held.
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

// feedSince returns feed items newer than seq, oldest first.
// o.mu must be held.
func (o *Observatory) feedSince(seq uint64) []feedItem {
	var out []feedItem
	oldest := o.feedSeq - uint64(len(o.feed)) + 1
	for s := max(seq+1, oldest); s <= o.feedSeq && len(o.feed) > 0; s++ {
		out = append(out, o.feed[int(s-1)%feedSize])
	}
	return out
}

// frame drains everything that happened since the previous frame. When
// withStage is set the frame also carries a full stage snapshot, which
// heals any client that missed frames.
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

// stageLocked lists occupied stage slots. o.mu must be held.
func (o *Observatory) stageLocked() []stageSlot {
	out := make([]stageSlot, 0, stageSize)
	for slot, id := range o.stage {
		if id == "" {
			continue
		}
		s := stageSlot{Slot: slot, ID: id, Name: lemmingName(id), State: "walking"}
		if l := o.lives[id]; l != nil {
			s.Terrain, s.State, s.QueuePos = l.Terrain, l.State, l.QueuePos
		}
		out = append(out, s)
	}
	return out
}

// setPhase records the swarm phase shown in the dashboard header.
func (o *Observatory) setPhase(phase string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.phase != "done" {
		o.phase = phase
	}
}

// life returns a copy of one lemming's life, and whether it is tracked.
func (o *Observatory) life(id string) (lemmingLife, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	l := o.lives[id]
	if l == nil {
		return lemmingLife{}, false
	}
	c := *l
	c.History = append([]lifeVisit(nil), l.History...)
	return c, true
}

// terrainTiles groups terrains into at most maxTerrainTiles tiles.
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

// snapshot returns everything a newly connected browser needs.
func (o *Observatory) snapshot() (float64, string, []stageSlot, []feedItem, uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.since(time.Now()), o.phase, o.stageLocked(), o.feedSince(0), o.feedSeq
}
