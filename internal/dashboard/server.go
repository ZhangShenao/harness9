// Package dashboard provides a local-first HTTP control console for the
// Mission Control Plane. It listens on 127.0.0.1 only and renders server-side
// HTML templates -- no SPA framework, no Node build chain, no external runtime.
package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/harness9/internal/mission"
)

// Server is the local Dashboard HTTP server.
type Server struct {
	store *mission.Store
	cs    *mission.CommandService
	addr  string
	mux   *http.ServeMux
}

// NewServer creates a Dashboard server bound to addr (e.g. "127.0.0.1:7777").
func NewServer(store *mission.Store, cs *mission.CommandService, addr string) *Server {
	s := &Server{store: store, cs: cs, addr: addr, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/missions/", s.handleMissionDetail)
	s.mux.HandleFunc("/command", s.handleCommand)
	s.mux.HandleFunc("/create-mission", s.handleCreateMission)
	s.mux.HandleFunc("/add-task", s.handleAddTask)
	s.mux.HandleFunc("/api/missions", s.handleAPIMissions)
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.addr, s.mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	missions, _ := s.listMissions(ctx)
	renderPage(w, "index", map[string]any{
		"Title":    "Mission Control",
		"Missions": missions,
	})
}

func (s *Server) handleMissionDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/missions/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	ctx := r.Context()
	m, err := s.store.GetMission(ctx, id)
	if err != nil {
		http.Error(w, "mission not found", http.StatusNotFound)
		return
	}
	tasks, _ := s.store.ListTasks(ctx, id)
	auditEvents, _ := s.store.ListAuditEvents(ctx, id)
	pendingCRs, _ := s.store.ListPendingChangeRequests(ctx, id)
	planVersions, _ := s.store.ListPlanVersions(ctx, id)
	var draftPlan *mission.Plan
	if pv, err := s.store.GetActivePlanVersion(ctx, id); err == nil {
		_ = pv
	}
	renderPage(w, "detail", map[string]any{
		"Title":        "Mission " + id[:8],
		"Mission":      m,
		"Tasks":        tasks,
		"AuditEvents":  auditEvents,
		"PendingCRs":   pendingCRs,
		"PlanVersions": planVersions,
		"DraftPlan":    draftPlan,
	})
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	cmd := mission.Command{
		Kind:           mission.CommandKind(r.FormValue("kind")),
		Actor:          "operator",
		Reason:         r.FormValue("reason"),
		IdempotencyKey: r.FormValue("idempotency_key"),
		Target:         r.FormValue("target"),
	}
	if cmd.IdempotencyKey == "" {
		cmd.IdempotencyKey = fmt.Sprintf("web-%d", r.ContentLength)
	}
	if tasksJSON := r.FormValue("tasks_json"); tasksJSON != "" {
		cmd.Payload = json.RawMessage(tasksJSON)
	}
	result := s.cs.Execute(r.Context(), cmd)
	if result.Error != nil {
		http.Error(w, result.Error.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, r.FormValue("redirect"), http.StatusFound)
}

func (s *Server) handleCreateMission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	goal := strings.TrimSpace(r.FormValue("goal"))
	if goal == "" {
		http.Error(w, "goal is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	m, err := s.store.CreateMission(ctx, mission.CreateMissionInput{Goal: goal})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.store.TransitionMission(ctx, m.ID, mission.MissionPlanning)
	http.Redirect(w, r, "/missions/"+m.ID, http.StatusFound)
}

func (s *Server) handleAddTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	missionID := r.FormValue("mission_id")
	title := strings.TrimSpace(r.FormValue("title"))
	if missionID == "" || title == "" {
		http.Error(w, "mission_id and title are required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	task, err := s.store.CreateTask(ctx, mission.CreateTaskInput{MissionID: missionID, Title: title})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	contractKind := r.FormValue("contract_kind")
	if contractKind == "" {
		contractKind = "implementation"
	}
	s.store.DB().ExecContext(ctx, `UPDATE tasks SET contract_kind = ? WHERE id = ?`, contractKind, task.ID)
	http.Redirect(w, r, "/missions/"+missionID, http.StatusFound)
}

func (s *Server) handleAPIMissions(w http.ResponseWriter, r *http.Request) {
	missions, err := s.listMissions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(missions)
}

type missionSummary struct {
	ID     string `json:"id"`
	Goal   string `json:"goal"`
	Status string `json:"status"`
	Tasks  int    `json:"tasks"`
}

func (s *Server) listMissions(ctx context.Context) ([]missionSummary, error) {
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT id, goal, status FROM missions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var missions []missionSummary
	for rows.Next() {
		var m missionSummary
		if err := rows.Scan(&m.ID, &m.Goal, &m.Status); err != nil {
			return nil, err
		}
		var count int
		s.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE mission_id = ?`, m.ID).Scan(&count)
		m.Tasks = count
		missions = append(missions, m)
	}
	return missions, nil
}

// OpenStore opens a mission Store on the given SQLite DB path.
func OpenStore(dbPath string) (*mission.Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return mission.NewStore(db)
}
