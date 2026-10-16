// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// P2Handler serves /v1/p2/<operation> on the separate admin listener (IDN-7).
func (s *Server) P2Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/p2/", s.p2)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "no such path"})
	})
	return mux
}

func (s *Server) operator(token string) (core.Operator, bool) {
	if token == "" {
		return core.Operator{}, false
	}
	h := sha256Hex(token)
	for _, o := range s.cfg.Operators {
		if equalHex(h, strings.ToLower(o.TokenSHA256)) {
			return core.Operator{Principal: o.Principal, Tenant: o.Tenant}, true
		}
	}
	return core.Operator{}, false
}

func (s *Server) isHealthKey(token string) bool {
	return keyIn(token, s.cfg.HealthKeysSHA256)
}

// reportOps are the operations a report key opens: the external backup process and the usage
// reconciliation process hand in their reports and nothing else (BUD-10, RET-8).
var reportOps = map[string]bool{"backup.report": true, "spend.reconcile": true}

// reportOperator is the operator a report key acts as: farm level, with no person behind it.
var reportOperator = core.Operator{Principal: "system", Tenant: "farm"}

func keyIn(token string, keys []string) bool {
	if token == "" {
		return false
	}
	h := sha256Hex(token)
	for _, k := range keys {
		if equalHex(h, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

func (s *Server) p2(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/p2/")
	info, ok := s.ops["P2."+op]
	if !ok {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "no such operation"})
		return
	}
	if r.Method != http.MethodPost {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "operations are POST"})
		return
	}
	ctx := r.Context()
	token := bearer(r)
	if s.isHealthKey(token) {
		if op != "health" {
			Problem(w, &core.Problem{Reason: "forbidden", Detail: "the health key opens only health"})
			return
		}
		if _, ok := s.body(w, r, info.Request); !ok {
			return
		}
		writeJSON(w, s.c.Health(ctx))
		return
	}
	oper, ok := s.operator(token)
	if !ok && keyIn(token, s.cfg.ReportKeysSHA256) {
		if !reportOps[op] {
			Problem(w, &core.Problem{Reason: "forbidden", Detail: "a report key opens only backup.report and spend.reconcile"})
			return
		}
		oper, ok = reportOperator, true
	}
	if !ok {
		Problem(w, &core.Problem{Reason: "unauthenticated", Detail: "unknown P2 credentials"})
		return
	}
	m, ok := s.body(w, r, info.Request)
	if !ok {
		return
	}
	if v, has := m["op"]; has && op != "control.apply" && op != "manager.direct" && v != op {
		Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "op does not match the path"})
		return
	}
	var out any
	var err error
	switch op {
	case "health":
		out = s.c.Health(ctx)
	case "apply":
		req, derr := decode[core.ApplyRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.Apply(ctx, oper, req)
	case "principal.add", "principal.off", "principal.on", "principal.set_identity":
		req, derr := decode[core.PrincipalRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.Principal(ctx, oper, req)
	case "principal.offboard":
		req, derr := decode[core.PrincipalRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		req.Op = op
		out, err = s.c.Principal(ctx, oper, req)
	case "grant", "ungrant":
		req, derr := decode[core.GrantRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.Grant(ctx, oper, req)
	case "worker.enroll":
		req, derr := decode[core.EnrollRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.Enroll(ctx, oper, req)
	case "worker.drain":
		req, derr := decode[core.WorkerRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.WorkerDrain(ctx, oper, req)
	case "secret.revoke":
		req, derr := decode[core.SecretRevokeRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.SecretRevoke(ctx, oper, req)
	case "worker.revoke":
		req, derr := decode[core.WorkerRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.WorkerRevoke(ctx, oper, req)
	case "bot.retire":
		req, derr := decode[core.BotRetireRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.BotRetire(ctx, oper, req)
	case "bot.list":
		out, err = s.c.BotList(ctx, oper)
	case "bot.show":
		id, _ := m["id"].(string)
		out, err = s.c.BotShow(ctx, oper, id)
	case "seat.assign", "seat.vacate":
		req, derr := decode[core.SeatRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		if op == "seat.assign" {
			out, err = s.c.SeatAssign(ctx, oper, req)
		} else {
			out, err = s.c.SeatVacate(ctx, oper, req)
		}
	case "seat.resume":
		req, derr := decode[core.SeatResumeRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.SeatResume(ctx, oper, req)
	case "control.apply":
		req, derr := decode[core.ControlRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ControlApply(ctx, oper, req)
	case "control.revert":
		req, derr := decode[core.ControlRevertRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ControlRevert(ctx, oper, req)
	case "control.list":
		req, derr := decode[core.TenantRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ControlList(ctx, oper, req)
	case "manager.direct":
		req, derr := decode[core.ManagerDirectRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ManagerDirect(ctx, oper, req)
	case "manager.cost":
		req, derr := decode[core.ManagerCostRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ManagerCost(ctx, oper, req)
	case "manager.reads":
		req, derr := decode[core.ManagerReadsRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ManagerReads(ctx, oper, req)
	case "incident.list":
		req, derr := decode[core.TenantRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.IncidentList(ctx, oper, req)
	case "incident.show", "incident.ack", "incident.close", "incident.dismiss":
		req, derr := decode[core.IncidentRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		switch op {
		case "incident.show":
			out, err = s.c.IncidentShow(ctx, oper, req)
		case "incident.ack":
			out, err = s.c.IncidentAck(ctx, oper, req)
		case "incident.close":
			out, err = s.c.IncidentClose(ctx, oper, req)
		default:
			out, err = s.c.IncidentDismiss(ctx, oper, req)
		}
	case "hold.set":
		req, derr := decode[core.HoldSetRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.HoldSet(ctx, core.OperatorActor(oper), req)
	case "hold.release":
		req, derr := decode[core.HoldReleaseRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.HoldRelease(ctx, core.OperatorActor(oper), req)
	case "hold.list":
		req, derr := decode[core.TenantRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.HoldList(ctx, core.OperatorActor(oper), req)
	case "status", "doctor", "deadletter.list":
		req, derr := decode[core.TenantRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		switch op {
		case "status":
			out, err = s.c.Status(ctx, oper, req)
		case "doctor":
			out, err = s.c.Doctor(ctx, oper, req)
		default:
			out, err = s.c.DeadletterList(ctx, oper, req)
		}
	case "task.redrive", "delivery.redrive":
		req, derr := decode[core.RedriveRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.Redrive(ctx, oper, req)
	case "spend.reconcile":
		req, derr := decode[core.SpendReconcileRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.SpendReconcile(ctx, oper, req)
	case "core.drain":
		req, derr := decode[core.CoreDrainRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.CoreDrain(ctx, oper, req)
	case "backup.report":
		req, derr := decode[core.BackupReportRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.BackupReport(ctx, oper, req)
	case "restore.complete":
		req, derr := decode[core.RestoreCompleteRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.RestoreComplete(ctx, oper, req)
	case "chat.bind":
		req, derr := decode[core.ChatBindRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ChatBind(ctx, oper, req)
	case "erasure.request":
		req, derr := decode[core.ErasureRequestInput](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ErasureRequest(ctx, oper, req)
	case "erasure.status", "erasure.retry":
		req, derr := decode[core.ErasureIDRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		if op == "erasure.status" {
			out, err = s.c.ErasureStatus(ctx, oper, req)
		} else {
			out, err = s.c.ErasureRetry(ctx, oper, req)
		}
	case "erasure.list":
		req, derr := decode[core.TenantRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ErasureList(ctx, oper, req)
	case "erasure.match":
		req, derr := decode[core.MatchRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ErasureMatch(ctx, oper, req)
	case "principal.data_request":
		req, derr := decode[core.DataRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.PrincipalDataRequest(ctx, oper, req)
	case "proposal.link_commit":
		req, derr := decode[core.LinkCommitRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.LinkCommit(ctx, oper, req)
	case "obs.series":
		req, derr := decode[core.ObsQueryRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.ObsSeries(ctx, oper, req)
	case "audit.query":
		req, derr := decode[core.AuditQueryRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.AuditQuery(ctx, oper, req)
	case "audit.verify":
		req, derr := decode[core.AuditVerifyRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		if req.Tenant != oper.Tenant {
			Problem(w, &core.Problem{Reason: "forbidden", Detail: "the operator credential is for another tenant"})
			return
		}
		out, err = s.c.AuditVerify(ctx, req.Tenant, req.Heads)
	default:
		Problem(w, &core.Problem{Reason: "not_found", Detail: "the operation is not implemented in this build"})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, out)
}
