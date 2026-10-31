package ctl

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"github.com/aisidev/tt/model"
	"github.com/google/uuid"
)

func (s *Server) controlReportHTTP(w http.ResponseWriter, r *http.Request) {
	report, err := s.commandJournal.ControlReport()
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	if report.Enabled {
		report.EnrollmentVersion = 1
		if s.enrollmentStore != nil {
			report.EnrollmentTokenVersion = 1
		}
	}
	reply(w, report, nil, 200)
}

// Enrollment records only the explicitly named receiver. Agent workloads and
// disks absent from controller ownership are observed, never adopted.
func (s *Server) enrollControlHTTP(w http.ResponseWriter, r *http.Request) {
	envelope, ok := command.FromContext(r.Context())
	if !ok || s.commandJournal.Authority == nil {
		reply(w, model.Unit{}, diskAPIError("terminal", "missing_identity", "enrollment requires current deployment authority"), 200)
		return
	}
	request, ok := api.ReadJSON[model.EnrollmentRequest](w, r)
	if !ok {
		return
	}
	parsed, err := url.Parse(request.Endpoint)
	scope, scopeErr := hex.DecodeString(request.ScopeDigest)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" || !validAgentAddress(parsed.Host) || scopeErr != nil || len(scope) != 32 || strings.ToLower(request.ScopeDigest) != request.ScopeDigest || model.ValidateName(request.HostID, "host_id") != nil {
		reply(w, model.Unit{}, diskAPIError("terminal", "invalid_receiver", "invalid explicit receiver endpoint or scope"), 200)
		return
	}
	a := s.commandJournal.Authority
	activation, err := a.Activation("mutations")
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	if activation == nil || activation.Request.PlanDigest != request.PlanDigest {
		reply(w, model.Unit{}, diskAPIError("state", "recovery_plan_conflict", "receiver enrollment differs from activated plan"), 200)
		return
	}
	// The private header is never reflected into a cached public receipt.
	token := r.Header.Get("X-Fleet-Enrollment-Token")
	if s.enrollmentStore != nil {
		if err := s.enrollmentStore.Check(token, envelope, request); err != nil {
			reply(w, model.Unit{}, internal(err), 200)
			return
		}
	}
	if s.enrollmentStore != nil {
		original, err := s.enrollmentStore.Receipt(token, envelope, request)
		if err != nil {
			reply(w, model.Unit{}, internal(err), 200)
			return
		}
		if original != nil {
			reply(w, *original, nil, 200)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	client, err := s.clientFactory(30 * time.Second)
	if err != nil {
		reply(w, model.Unit{}, internal(err), 200)
		return
	}
	response, err := client.request(ctx, "GET", parsed.Host, "/api/control", nil, nil)
	if err != nil {
		reply(w, model.Unit{}, diskAPIError("unknown", "receiver_unavailable", "cannot verify receiver control authority"), 200)
		return
	}
	status := response.StatusCode
	control, err := decodeResponse[model.ControlReport](response)
	if err != nil || !successful(status) || !control.OK || control.Data == nil {
		reply(w, model.Unit{}, diskAPIError("unknown", "receiver_unavailable", "receiver control report unavailable"), 200)
		return
	}
	report := control.Data
	if !report.Enabled || report.Authority != envelope.Authority || report.Epoch != envelope.Epoch || (report.Phase != "mutations" && report.Phase != "gc") || report.ScopeDigest != request.ScopeDigest || report.Fence == nil || report.Fence.HostID != request.HostID {
		reply(w, model.Unit{}, diskAPIError("state", "identity_mismatch", "receiver does not match activated deployment scope"), 200)
		return
	}
	verified := false
	for _, receipt := range report.Activations {
		if receipt.Request.Phase == "mutations" && receipt.Authority == envelope.Authority && receipt.Epoch == envelope.Epoch && receipt.HostID == request.HostID && receipt.ScopeDigest == request.ScopeDigest && receipt.Request.PlanDigest == request.PlanDigest {
			verified = true
		}
	}
	if !verified {
		reply(w, model.Unit{}, diskAPIError("state", "activation_unconfirmed", "receiver has no matching activation evidence"), 200)
		return
	}
	if s.enrollmentStore != nil {
		if err := s.enrollmentStore.Claim(token, envelope, request); err != nil {
			reply(w, model.Unit{}, internal(err), 200)
			return
		}
	}
	guard := func() *APIError {
		current, err := a.State()
		if err != nil {
			return internal(err)
		}
		if current.ID != envelope.Authority || current.Epoch != envelope.Epoch {
			return diskAPIError("state", "stale_epoch", "enrollment authority changed before membership write")
		}
		if err = a.RequireActive("mutations"); err != nil {
			return internal(err)
		}
		activation, err := a.Activation("mutations")
		if err != nil {
			return internal(err)
		}
		if activation == nil || activation.Request.PlanDigest != request.PlanDigest {
			return diskAPIError("state", "recovery_plan_conflict", "enrollment plan changed before membership write")
		}
		return nil
	}
	if _, _, apiErr := s.registerHostExpected(ctx, api.RegisterHostReq{Addr: parsed.Host}, request.HostID, guard); apiErr != nil {
		reply(w, model.Unit{}, apiErr, 200)
		return
	}
	receipt := model.EnrollmentReceipt{Version: 1, ID: uuid.NewString(), Authority: envelope.Authority, Epoch: envelope.Epoch, Key: envelope.Key, Request: request}
	if s.enrollmentStore != nil {
		if err := s.enrollmentStore.Complete(token, envelope, request, receipt); err != nil {
			reply(w, model.Unit{}, internal(err), 200)
			return
		}
	}
	reply(w, receipt, nil, 200)
}
