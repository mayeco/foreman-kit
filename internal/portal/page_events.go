package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/mayeco/foreman-kit/internal/store"
)

// Events: what the orchestrator recorded, as it recorded it. /events lists them and /events/{id}
// shows one; /api/events and /api/events/{id} return the same reads. An event names the rows it is
// about, and the list is narrowed by those columns: every filter is a search on an index.

// eventsFilter is what a page of events was narrowed to. A field left empty does not filter.
type eventsFilter struct {
	Event          string `json:"event,omitempty"`
	Change         string `json:"change,omitempty"`
	ExecID         string `json:"exec_id,omitempty"`
	ChangeRunID    int64  `json:"change_run_id,omitempty"`
	MergeAttemptID int64  `json:"merge_attempt_id,omitempty"`
	GateRoundID    int64  `json:"gate_round_id,omitempty"`
	CycleStamp     string `json:"cycle_stamp,omitempty"`
}

// narrowing is one filter in force, as the page says it: what it keeps, the page of the row it
// names when that row has one, and the address of the list without it.
type narrowing struct {
	Label   string
	Value   string
	Href    string
	Without string
}

// eventsPage is one page of events, the latest first.
type eventsPage struct {
	Items  []store.EventRecord `json:"items"`
	Next   string              `json:"next,omitempty"`
	Filter eventsFilter        `json:"filter"`

	// Types holds the types of event there are under the other filters, with how many of each;
	// Total, how many events that is.
	Types    []store.EventTypeCount `json:"-"`
	Total    int                    `json:"-"`
	Narrowed []narrowing            `json:"-"`
	Older    string                 `json:"-"`
	Newest   string                 `json:"-"`
}

func (p *eventsPage) head(v *pageView) {
	v.Title, v.Section = "Events", "/events"
	v.Crumbs = []crumb{{Label: "Events"}}
}

// idParam reads a parameter that names a row by its number: 0 when it is not given.
func idParam(q url.Values, key string) (int64, error) {
	raw := strings.TrimSpace(q.Get(key))
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errInvalid("%s is the number of a row, and %q is not one", key, raw)
	}
	return id, nil
}

func (s *Server) readEvents(ctx context.Context, db *store.View, r *http.Request) (*eventsPage, error) {
	q := r.URL.Query()
	limit, err := limitOf(q)
	if err != nil {
		return nil, err
	}
	f := eventsFilter{
		Event: strings.TrimSpace(q.Get("event")), Change: strings.TrimSpace(q.Get("change")),
		ExecID: strings.TrimSpace(q.Get("exec_id")), CycleStamp: strings.TrimSpace(q.Get("cycle_stamp")),
	}
	if f.ChangeRunID, err = idParam(q, "change_run_id"); err != nil {
		return nil, err
	}
	if f.MergeAttemptID, err = idParam(q, "merge_attempt_id"); err != nil {
		return nil, err
	}
	if f.GateRoundID, err = idParam(q, "gate_round_id"); err != nil {
		return nil, err
	}
	before := strings.TrimSpace(q.Get("before"))
	filter := store.EventPageFilter{
		Event: f.Event, Change: f.Change, ExecID: f.ExecID, ChangeRunID: f.ChangeRunID, MergeAttemptID: f.MergeAttemptID,
		GateRoundID: f.GateRoundID, CycleStamp: f.CycleStamp, Before: before, Limit: limit,
	}
	page, err := db.ListEventsPage(ctx, filter)
	if err != nil {
		return nil, err
	}
	p := &eventsPage{Items: page.Items, Next: page.Next, Filter: f}
	if p.Items == nil {
		p.Items = []store.EventRecord{}
	}
	if p.Types, err = db.CountEventsByType(ctx, filter); err != nil {
		return nil, err
	}
	for _, t := range p.Types {
		p.Total += t.Count
	}

	narrow := func(label, key, value, href string) {
		if value != "" && value != "0" {
			p.Narrowed = append(p.Narrowed, narrowing{Label: label, Value: value, Href: href, Without: hrefWith(r, key, "", "before", "")})
		}
	}
	narrow("change", "change", f.Change, "/changes/"+url.PathEscape(f.Change))
	narrow("execution", "exec_id", f.ExecID, executionHref(f.ExecID))
	narrow("worker run", "change_run_id", strconv.FormatInt(f.ChangeRunID, 10), "")
	narrow("merge attempt", "merge_attempt_id", strconv.FormatInt(f.MergeAttemptID, 10), "")
	narrow("round of gates", "gate_round_id", strconv.FormatInt(f.GateRoundID, 10), "/gates/"+strconv.FormatInt(f.GateRoundID, 10))
	narrow("cycle", "cycle_stamp", f.CycleStamp, "/cycles/"+url.PathEscape(f.CycleStamp))
	if page.Next != "" {
		p.Older = hrefWith(r, "before", page.Next)
	}
	if before != "" {
		p.Newest = hrefWith(r, "before", "")
	}
	return p, nil
}

// payloadField is one field of what an event carries, as text.
type payloadField struct {
	Name  string
	Value string
}

// eventLink leads from an event to a row it is about.
type eventLink struct {
	Label string
	Href  string
}

// eventPage is one event.
type eventPage struct {
	store.EventRecord

	// Fields holds what the event carries besides its name and its time, by name; Raw, the whole of
	// it as it was recorded. Links lead to the rows it names, and Lists to the events of each.
	Fields []payloadField `json:"-"`
	Raw    string         `json:"-"`
	Links  []eventLink    `json:"-"`
	Lists  []eventLink    `json:"-"`
}

func (p *eventPage) head(v *pageView) {
	v.Title, v.Section = p.Event+" · event #"+strconv.FormatInt(p.ID, 10), "/events"
	v.Crumbs = []crumb{{Label: "Events", Href: "/events"}, {Label: "#" + strconv.FormatInt(p.ID, 10)}}
}

func (s *Server) readEvent(ctx context.Context, db *store.View, r *http.Request) (*eventPage, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return nil, errNotFound("no event has the number %s", r.PathValue("id"))
	}
	rec, err := db.Event(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errNotFound("no event has the number %d", id)
	}
	p := &eventPage{EventRecord: *rec}

	names := make([]string, 0, len(rec.Payload))
	for name := range rec.Payload {
		// The name, the time and the number of the event are its heading.
		if name != "event" && name != "ts" && name != "id" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		p.Fields = append(p.Fields, payloadField{Name: name, Value: eventField(rec.Payload, name)})
	}
	if raw, err := json.MarshalIndent(rec.Payload, "", "  "); err == nil {
		p.Raw = string(raw)
	}

	link := func(label, href string) { p.Links = append(p.Links, eventLink{Label: label, Href: href}) }
	list := func(label, key, value string) {
		p.Lists = append(p.Lists, eventLink{Label: label, Href: "/events?" + url.Values{key: {value}}.Encode()})
	}
	list("Every "+rec.Event+" event", "event", rec.Event)
	if rec.Change != "" {
		change := "/changes/" + url.PathEscape(rec.Change)
		link("The change "+rec.Change, change)
		if rec.ChangeRunID > 0 {
			link(fmt.Sprintf("Its worker run #%d", rec.ChangeRunID), fmt.Sprintf("%s#run-%d", change, rec.ChangeRunID))
		}
		if rec.MergeAttemptID > 0 {
			link(fmt.Sprintf("Its merge attempt #%d", rec.MergeAttemptID), fmt.Sprintf("%s#merge-%d", change, rec.MergeAttemptID))
		}
		list("The events of "+rec.Change, "change", rec.Change)
	}
	if rec.ExecID != "" {
		link("The execution it belongs to", executionHref(rec.ExecID))
		list("The events of that execution", "exec_id", rec.ExecID)
	}
	if rec.GateRoundID > 0 {
		link(fmt.Sprintf("The round of gates #%d", rec.GateRoundID), fmt.Sprintf("/gates/%d", rec.GateRoundID))
	}
	if rec.CycleStamp != "" {
		link("The cycle "+rec.CycleStamp, "/cycles/"+url.PathEscape(rec.CycleStamp))
		list("The events of that cycle", "cycle_stamp", rec.CycleStamp)
	}
	return p, nil
}
