package dashboard

import (
	"html/template"
	"net/http"
)

const layoutHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} - harness9 Mission Control</title>
<style>
:root { --bg:#1a1a2e; --card:#16213e; --accent:#0f3460; --text:#e0e0e0; --green:#4ecca3; --yellow:#f5d76e; --red:#e94560; --blue:#4a9eff; }
* { margin:0; padding:0; box-sizing:border-box; }
body { font-family:system-ui,-apple-system,sans-serif; background:var(--bg); color:var(--text); padding:20px; max-width:1200px; margin:0 auto; }
h1 { margin-bottom:20px; font-size:1.6rem; }
h2 { margin:20px 0 10px; font-size:1.2rem; color:var(--blue); }
a { color:var(--blue); text-decoration:none; }
a:hover { text-decoration:underline; }
.card { background:var(--card); border-radius:8px; padding:16px; margin-bottom:12px; border:1px solid var(--accent); }
.badge { display:inline-block; padding:2px 10px; border-radius:12px; font-size:0.8rem; font-weight:600; }
.badge.draft { background:#333; color:#999; }
.badge.planning { background:#444; color:var(--yellow); }
.badge.ready { background:#2a4a2a; color:var(--green); }
.badge.running { background:#2a3a5a; color:var(--blue); }
.badge.verifying { background:#4a4a2a; color:var(--yellow); }
.badge.succeeded { background:#1a4a2a; color:var(--green); }
.badge.failed { background:#4a1a1a; color:var(--red); }
.badge.needs_attention { background:#4a2a1a; color:var(--yellow); }
.badge.cancelled { background:#333; color:#666; }
.badge.blocked { background:#333; color:#999; }
.badge.queued { background:#2a3a5a; color:var(--blue); }
.badge.leased { background:#2a4a5a; color:var(--blue); }
.badge.succeeded { background:#1a4a2a; color:var(--green); }
table { width:100%; border-collapse:collapse; margin:10px 0; }
th,td { text-align:left; padding:8px 12px; border-bottom:1px solid var(--accent); }
th { color:var(--blue); font-size:0.85rem; text-transform:uppercase; }
.btn { display:inline-block; padding:6px 16px; border-radius:6px; border:none; cursor:pointer; font-size:0.9rem; margin:2px; }
.btn-green { background:var(--green); color:#000; }
.btn-red { background:var(--red); color:#fff; }
.btn-blue { background:var(--blue); color:#000; }
form { display:inline; }
pre { background:#0d1117; padding:12px; border-radius:6px; overflow-x:auto; font-size:0.85rem; }
.empty { color:#666; font-style:italic; padding:20px; text-align:center; }
.nav { margin-bottom:20px; }
.nav a { margin-right:16px; }
</style>
</head>
<body>
<div class="nav"><a href="/">Dashboard</a> <a href="https://github.com/ZhangShenao/harness9" target="_blank">GitHub</a></div>
{{template "content" .}}
</body>
</html>`

const indexHTML = `{{define "content"}}
<h1>Mission Control</h1>
{{if .Missions}}
<table>
<tr><th>ID</th><th>Goal</th><th>Status</th><th>Tasks</th></tr>
{{range .Missions}}
<tr>
<td><a href="/missions/{{.ID}}">{{.ID}}</a></td>
<td>{{.Goal}}</td>
<td><span class="badge {{.Status}}">{{.Status}}</span></td>
<td>{{.Tasks}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="empty">No missions yet. Use <code>/mission &lt;goal&gt;</code> in the TUI to create one.</p>
{{end}}
{{end}}`

const detailHTML = `{{define "content"}}
<h1>{{.Title}}</h1>
<div class="card">
<p><strong>Goal:</strong> {{.Mission.Goal}}</p>
<p><strong>Status:</strong> <span class="badge {{.Mission.Status}}">{{.Mission.Status}}</span></p>
<p><strong>Created:</strong> {{.Mission.CreatedAt.Format "2006-01-02 15:04:05"}}</p>
</div>

<h2>Tasks</h2>
{{if .Tasks}}
<table>
<tr><th>ID</th><th>Title</th><th>Status</th><th>Contract</th><th>Deps</th></tr>
{{range .Tasks}}
<tr>
<td>{{.ID}}</td>
<td>{{.Title}}</td>
<td><span class="badge {{.Status}}">{{.Status}}</span></td>
<td>{{.ContractKind}}</td>
<td>{{len .DependsOn}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="empty">No tasks.</p>
{{end}}

<h2>Commands</h2>
<div class="card">
<form method="POST" action="/command">
<input type="hidden" name="kind" value="pause_mission">
<input type="hidden" name="target" value="{{.Mission.ID}}">
<input type="hidden" name="redirect" value="/missions/{{.Mission.ID}}">
<input type="hidden" name="idempotency_key" value="pause-{{.Mission.ID}}">
<button class="btn btn-blue" type="submit">Pause</button>
</form>
<form method="POST" action="/command">
<input type="hidden" name="kind" value="resume_mission">
<input type="hidden" name="target" value="{{.Mission.ID}}">
<input type="hidden" name="redirect" value="/missions/{{.Mission.ID}}">
<input type="hidden" name="idempotency_key" value="resume-{{.Mission.ID}}">
<button class="btn btn-green" type="submit">Resume</button>
</form>
<form method="POST" action="/command">
<input type="hidden" name="kind" value="cancel_mission">
<input type="hidden" name="target" value="{{.Mission.ID}}">
<input type="hidden" name="redirect" value="/">
<input type="hidden" name="idempotency_key" value="cancel-{{.Mission.ID}}">
<button class="btn btn-red" type="submit" onclick="return confirm('Cancel this mission?')">Cancel</button>
</form>
</div>

{{if .PendingCRs}}
<h2>Pending Change Requests</h2>
{{range .PendingCRs}}
<div class="card">
<p><strong>Reason:</strong> {{.Reason}}</p>
<form method="POST" action="/command" style="display:inline">
<input type="hidden" name="kind" value="approve_change_request">
<input type="hidden" name="target" value="{{.ID}}">
<input type="hidden" name="redirect" value="/missions/{{.Mission.ID}}">
<button class="btn btn-green" type="submit">Approve</button>
</form>
<form method="POST" action="/command" style="display:inline">
<input type="hidden" name="kind" value="reject_change_request">
<input type="hidden" name="target" value="{{.ID}}">
<input type="hidden" name="redirect" value="/missions/{{.Mission.ID}}">
<button class="btn btn-red" type="submit">Reject</button>
</form>
</div>
{{end}}
{{end}}

<h2>Audit Trail</h2>
{{if .AuditEvents}}
<table>
<tr><th>Time</th><th>Command</th><th>Actor</th><th>Result</th><th>Reason</th></tr>
{{range .AuditEvents}}
<tr>
<td>{{.CreatedAt.Format "15:04:05"}}</td>
<td>{{.CommandKind}}</td>
<td>{{.Actor}}</td>
<td><span class="badge {{if eq .Result "applied"}}succeeded{{else}}failed{{end}}">{{.Result}}</span></td>
<td>{{.Reason}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="empty">No audit events.</p>
{{end}}
{{end}}`

var layoutTmpl = template.Must(template.New("layout").Parse(layoutHTML))

func renderPage(w http.ResponseWriter, name string, data map[string]any) {
	var pageContent string
	switch name {
	case "index":
		pageContent = indexHTML
	case "detail":
		pageContent = detailHTML
	default:
		http.Error(w, "unknown template: "+name, http.StatusInternalServerError)
		return
	}
	t := template.Must(layoutTmpl.Clone())
	template.Must(t.Parse(pageContent))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
