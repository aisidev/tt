package ctl

import (
	"context"
	"errors"
	"fmt"
	"github.com/aisidev/tt/api"
	"github.com/aisidev/tt/internal/command"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type APIError struct {
	Status  int
	Message string
	Failure *api.BoundaryError
}

func (e *APIError) Error() string { return e.Message }
func apiError(status int, message string) *APIError {
	return &APIError{Status: status, Message: message}
}
func internal(err error) *APIError {
	if err == nil {
		return nil
	}
	var typed *api.BoundaryError
	if errors.As(err, &typed) {
		return &APIError{Status: typed.Status(), Message: typed.Message, Failure: typed}
	}
	return apiError(500, err.Error())
}

type Server struct {
	enrollmentStore *command.EnrollmentStore
	commandJournal  *command.Journal
	db              *DB
	dbMu            sync.Mutex
	operationsMu    sync.Mutex
	operations      map[string]bool
	revision        atomic.Uint64
	apiKey          *string
	stderr          io.Writer
	workCtx         context.Context
	workers         sync.WaitGroup
	now             func() uint64
	clientFactory   func(time.Duration) (*agentClient, error)
}

func newServer(db *DB, key *string) *Server {
	s := &Server{db: db, operations: map[string]bool{}, apiKey: key, stderr: db.stderr, workCtx: context.Background(), now: func() uint64 {
		n := time.Now().Unix()
		if n < 0 {
			return 0
		}
		return uint64(n)
	}}
	s.commandJournal = command.New(db.conn, &s.dbMu)
	s.clientFactory = func(timeout time.Duration) (*agentClient, error) { return newAgentClient(key, timeout) }
	return s
}

type operation struct {
	server *Server
	id     string
	once   sync.Once
}

func (s *Server) beginOperation(id string) (*operation, *APIError) {
	s.operationsMu.Lock()
	if s.operations[id] {
		s.operationsMu.Unlock()
		return nil, apiError(409, "environment has an operation in progress; inspect it before retrying")
	}
	s.operations[id] = true
	s.operationsMu.Unlock()
	s.dbMu.Lock()
	s.revision.Add(1)
	s.dbMu.Unlock()
	return &operation{server: s, id: id}, nil
}
func (s *Server) busy(id string) bool {
	s.operationsMu.Lock()
	defer s.operationsMu.Unlock()
	return s.operations[id]
}
func (o *operation) Close() {
	o.once.Do(func() {
		s := o.server
		s.dbMu.Lock()
		s.revision.Add(1)
		s.dbMu.Unlock()
		s.operationsMu.Lock()
		delete(s.operations, o.id)
		s.operationsMu.Unlock()
	})
}
func withDB[T any](s *Server, work func(*DB) (T, error)) (T, error) {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	return work(s.db)
}
func (s *Server) log(format string, args ...any) { fmt.Fprintf(s.stderr, format+"\n", args...) }
