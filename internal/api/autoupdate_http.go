package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/prezmc/tainerhub/internal/store"
	"github.com/prezmc/tainerhub/internal/wire"
)

// Automatic updates API (admins). Changes, including "Run now", need a
// recent sign-in and refuse API tokens (s.fresh): an opt-in makes the hub
// replace containers unattended.

type autoSettingsView struct {
	Settings autoSettings `json:"settings"`
	Zone     string       `json:"zone"` // the zone the window is in, for display
	Next     *time.Time   `json:"next"` // the next window's start
	NextText string       `json:"nextText,omitempty"`
}

func (s *Server) autoSettingsView(set autoSettings) autoSettingsView {
	v := autoSettingsView{Settings: set, Zone: set.zoneName()}
	if n := s.nextAutoWindow(set); !n.IsZero() {
		loc, err := set.location()
		if err != nil {
			loc = time.Local
		}
		n = n.In(loc)
		v.Next, v.NextText = &n, n.Format("Mon 2 Jan 15:04 MST")
	}
	return v
}

func (s *Server) getAutoSettings(w http.ResponseWriter, r *http.Request) {
	set, err := s.loadAutoSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.autoSettingsView(set))
}

func (s *Server) putAutoSettings(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	var in autoSettings
	if !readJSON(w, r, &in) {
		return
	}
	if err := in.validate(); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := json.Marshal(in)
	if err := s.aus().SetSetting(r.Context(), settingAutoUpdates, string(b)); err != nil {
		s.fail(w, err)
		return
	}
	s.useAutoSettings(in)
	u := userFrom(r)
	s.record(r, &u, "update.auto_settings", "automatic updates", in)
	writeJSON(w, http.StatusOK, s.autoSettingsView(in))
}

// listAutoUpdates answers GET /api/auto-updates: the opted-in targets with
// their last outcome, the window, and the current or last run.
func (s *Server) listAutoUpdates(w http.ResponseWriter, r *http.Request) {
	set, err := s.loadAutoSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	ts, err := s.aus().ListAutoUpdateTargets(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	// A host admin sees their hosts' targets, and the run's outcomes on them.
	u := userFrom(r)
	run := s.latestAutoRun()
	if run != nil && u.Scoped {
		run.Outcomes = visible(u, run.Outcomes, func(o autoOutcome) string { return o.HostID })
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": visible(u, nonNil(ts), func(t store.AutoUpdateTarget) string { return t.HostID }), "window": s.autoSettingsView(set), "run": run})
}

func (s *Server) latestAutoRunHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"run": s.latestAutoRun()})
}

// runAutoUpdatesNow runs the whole procedure now, even while the schedule
// is paused, with the usual window length as its limit.
func (s *Server) runAutoUpdatesNow(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	set, err := s.loadAutoSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	u := userFrom(r)
	run, err := s.startAutoRun("manual", u.Username, s.autoNow().Add(set.maxDuration()))
	if errors.Is(err, errAutoBusy) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "runId": run.ID})
		return
	}
	s.record(r, &u, "update.auto_run", "automatic updates", map[string]string{"run": run.ID})
	writeJSON(w, http.StatusAccepted, map[string]string{"runId": run.ID})
}

// autoToggle reads {"enabled": true|false}.
func autoToggle(w http.ResponseWriter, r *http.Request) (bool, bool) {
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if !readJSON(w, r, &in) {
		return false, false
	}
	if in.Enabled == nil {
		jsonErr(w, http.StatusBadRequest, `send {"enabled": true} or {"enabled": false}`)
		return false, false
	}
	return *in.Enabled, true
}

// stackAutoUpdate opts a managed stack in or out.
func (s *Server) stackAutoUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	id := r.PathValue("sid")
	if !validUUID(id) {
		jsonErr(w, http.StatusNotFound, "stack not found")
		return
	}
	// {"enabled": true|false, "autoRollback": true|false}; autoRollback
	// ("Roll back automatically if the update fails") is optional.
	var in struct {
		Enabled      *bool `json:"enabled"`
		AutoRollback *bool `json:"autoRollback"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		jsonErr(w, http.StatusBadRequest, `send {"enabled": true} or {"enabled": false}`)
		return
	}
	on := *in.Enabled
	db := s.aus()
	st, err := db.GetStack(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		jsonErr(w, http.StatusNotFound, "stack not found")
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	hostName := st.HostID
	if h, err := db.GetHost(r.Context(), st.HostID); err == nil {
		hostName = h.Name
	}
	u := userFrom(r)
	if on {
		if msg := s.stackAutoRefusal(st); msg != "" {
			jsonErr(w, http.StatusConflict, msg)
			return
		}
		err = db.EnableStackAutoUpdate(r.Context(), st.ID, u.Username)
		if err == nil && in.AutoRollback != nil {
			_, err = db.SetStackAutoRollback(r.Context(), st.ID, *in.AutoRollback)
		}
	} else {
		_, err = db.DisableStackAutoUpdate(r.Context(), st.ID)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	detail := map[string]any{"kind": "stack", "host": hostName}
	res := map[string]bool{"enabled": on}
	if on {
		// The saved value (on unless turned off), for the response and the log.
		roll := true
		if ts, err := db.ListAutoUpdateTargets(r.Context()); err == nil {
			for _, t := range ts {
				if t.Kind == "stack" && t.StackID == st.ID {
					roll = t.AutoRollback
				}
			}
		}
		detail["autoRollback"], res["autoRollback"] = roll, roll
	}
	s.record(r, &u, autoToggleAction(on), hostName+"/"+st.Name, detail)
	writeJSON(w, http.StatusOK, res)
}

// stackAutoRefusal is why a stack can't be opted in ("" if it can).
// TainerHub's own project is refused; when the host is offline that can't
// be checked here, and the runner checks again before every update.
func (s *Server) stackAutoRefusal(st store.Stack) string {
	a := s.Reg.Get(st.HostID)
	if a == nil {
		return ""
	}
	status := a.Status()
	own := ownProjectOf(status)
	if own != "" && strings.EqualFold(own, st.Name) {
		return st.Name + " is TainerHub's own project: update TainerHub from Hosts → Updates"
	}
	for _, c := range status.Containers {
		if c.Project == st.Name && ownContainer(c, own) {
			return st.Name + " runs TainerHub's own containers: update TainerHub from Hosts → Updates"
		}
	}
	return ""
}

func autoToggleAction(on bool) string {
	if on {
		return "update.auto_enabled"
	}
	return "update.auto_disabled"
}

// containerAutoUpdate opts a standalone container in or out, by name (its
// ID changes with every update). Turning it on needs the host online, to
// check the container exists and isn't part of a compose project.
func (s *Server) containerAutoUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.fresh(w, r) {
		return
	}
	hostID, name := r.PathValue("id"), r.PathValue("name")
	if !validUUID(hostID) {
		jsonErr(w, http.StatusNotFound, "host not found")
		return
	}
	if !wire.ValidContainerRef(name) {
		jsonErr(w, http.StatusBadRequest, "invalid container name")
		return
	}
	on, ok := autoToggle(w, r)
	if !ok {
		return
	}
	db := s.aus()
	h, err := db.GetHost(r.Context(), hostID)
	if err != nil {
		s.notFoundOr(w, err)
		return
	}
	u := userFrom(r)
	if on {
		if status, msg := s.containerAutoRefusal(hostID, name); msg != "" {
			jsonErr(w, status, msg)
			return
		}
		err = db.EnableContainerAutoUpdate(r.Context(), hostID, name, u.Username)
	} else {
		_, err = db.DisableContainerAutoUpdate(r.Context(), hostID, name)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.record(r, &u, autoToggleAction(on), h.Name+"/"+name, map[string]string{"kind": "container", "host": h.Name})
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": on})
}

// containerAutoRefusal is why a container can't be opted in (status and
// message; "" if it can).
func (s *Server) containerAutoRefusal(hostID, name string) (int, string) {
	if strings.HasPrefix(name, "tainerhub") {
		return http.StatusConflict, "TainerHub's own containers can't be updated automatically: update TainerHub from Hosts → Updates"
	}
	a := s.Reg.Get(hostID)
	if a == nil {
		return http.StatusServiceUnavailable, "host is offline: TainerHub needs to see the container to check it can be updated on its own"
	}
	st := a.Status()
	for _, c := range st.Containers {
		if c.Name != name {
			continue
		}
		switch {
		case ownContainer(c, ownProjectOf(st)):
			return http.StatusConflict, "TainerHub's own containers can't be updated automatically: update TainerHub from Hosts → Updates"
		case c.Project != "":
			return http.StatusConflict, name + " belongs to the compose project " + c.Project + ": containers in a stack update together, so turn on automatic updates for the stack instead (a project started outside TainerHub has to be taken over first)"
		case !updateableRef(c.Image):
			return http.StatusBadRequest, name + " uses a pinned image (by digest or ID), so there's never a newer one to update to"
		}
		return 0, ""
	}
	return http.StatusNotFound, "no container called " + name + " on this host"
}
