package main

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/pielab-ai/pie-relay/executor-manager/internal/chatgateway"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/control"
	"github.com/pielab-ai/pie-relay/executor-manager/internal/manager"
)

type chatLifecycleOptions struct {
	ScanInterval        time.Duration
	SessionIdleTimeout  time.Duration
	ExecutorIdleTimeout time.Duration
}

type chatLifecycleStats struct {
	SessionsClosed   uint64
	ExecutorsStopped uint64
	Errors           uint64
}

type chatLifecycle struct {
	manager *manager.Manager
	control *control.Service
	chat    *chatgateway.Gateway
	options chatLifecycleOptions
	closed  atomic.Uint64
	stopped atomic.Uint64
	errors  atomic.Uint64
}

func newChatLifecycle(m *manager.Manager, service *control.Service, chat *chatgateway.Gateway, options chatLifecycleOptions) *chatLifecycle {
	return &chatLifecycle{manager: m, control: service, chat: chat, options: options}
}

func (l *chatLifecycle) Run(ctx context.Context) {
	ticker := time.NewTicker(l.options.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := l.Sweep(ctx, now.UTC()); err != nil {
				l.errors.Add(1)
				log.Printf("chat idle lifecycle: %v", err)
			}
		}
	}
}

func (l *chatLifecycle) Stats() chatLifecycleStats {
	return chatLifecycleStats{SessionsClosed: l.closed.Load(), ExecutorsStopped: l.stopped.Load(), Errors: l.errors.Load()}
}

func (l *chatLifecycle) Sweep(ctx context.Context, now time.Time) error {
	var results []error
	sessionCutoff := now.Add(-l.options.SessionIdleTimeout)
	for _, conversation := range l.control.Conversations() {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(results, err)...)
		}
		claimed, ok, err := l.chat.BeginIdle(conversation.ID, sessionCutoff)
		if err != nil {
			if !errors.Is(err, control.ErrNotFound) {
				results = append(results, err)
			}
			continue
		}
		if !ok {
			continue
		}
		_ = l.chat.RecordLifecycle(claimed.ID, "conversation.idle", "오랫동안 사용하지 않아 Claude 세션을 종료했습니다. 다시 연결하면 같은 대화를 계속 사용할 수 있습니다.")
		l.chat.Close(claimed.ID)
		if err := l.manager.StopSession(ctx, claimed.OwnerUserID, claimed.SessionID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
			if executor, exists := l.manager.Executor(claimed.OwnerUserID); !exists || executor.Status != "stopped" {
				results = append(results, err)
			}
		}
		if err := closeIdleSession(ctx, l.control, claimed.SessionID); err != nil {
			results = append(results, err)
		}
		if err := closeIdleConversation(ctx, l.control, claimed.ID); err != nil {
			results = append(results, err)
			continue
		}
		log.Printf("chat_session_closed reason=idle_timeout conversation_id=%s session_id=%s owner_user_id=%s", claimed.ID, claimed.SessionID, claimed.OwnerUserID)
		l.closed.Add(1)
	}

	executorCutoff := now.Add(-l.options.ExecutorIdleTimeout)
	for _, executor := range l.manager.Executors() {
		if executor.Status == "stopped" || !executorIdle(l.control, executor, executorCutoff) {
			continue
		}
		if _, err := l.manager.StopExecutor(ctx, executor.UserID); err != nil && !errors.Is(err, manager.ErrExecutorNotFound) {
			results = append(results, err)
			continue
		}
		l.stopped.Add(1)
	}
	return errors.Join(results...)
}

func closeIdleSession(ctx context.Context, service *control.Service, sessionID string) error {
	for range 8 {
		session, ok := service.Session(sessionID)
		if !ok {
			return nil
		}
		session.Status, session.LastError, session.HostConnectionID = "closed", "idle timeout", ""
		session.DriverUserID, session.DriverLeaseExpiresAt = "", nil
		if _, err := service.PutSession(ctx, session, session.Version, control.MutationMeta{ActorUserID: "chat-idle-reaper", SkipAudit: true}); err == nil {
			return nil
		} else if !errors.Is(err, control.ErrConflict) {
			return err
		}
	}
	return control.ErrConflict
}

func closeIdleConversation(ctx context.Context, service *control.Service, conversationID string) error {
	for range 8 {
		conversation, ok := service.Conversation(conversationID)
		if !ok || conversation.Status == "closed" || conversation.Status == "deleted" {
			return nil
		}
		conversation.Status, conversation.LastError = "closed", "idle timeout"
		if _, err := service.PutConversation(ctx, conversation, conversation.Version, control.MutationMeta{ActorUserID: "chat-idle-reaper", SkipAudit: true}); err == nil {
			return nil
		} else if !errors.Is(err, control.ErrConflict) {
			return err
		}
	}
	return control.ErrConflict
}

func executorIdle(service *control.Service, executor manager.Executor, cutoff time.Time) bool {
	latest := executor.LastUsedAt
	for _, project := range service.Projects() {
		if project.OwnerUserID != executor.UserID {
			continue
		}
		if project.Status != "ready" && project.Status != "failed" && project.Status != "deleted" {
			return false
		}
		if project.UpdatedAt.After(latest) {
			latest = project.UpdatedAt
		}
	}
	for _, session := range service.Sessions() {
		if session.OwnerUserID == executor.UserID && session.ExecutionTarget == "docker" && session.Status != "closed" && session.Status != "error" {
			return false
		}
	}
	for _, conversation := range service.Conversations() {
		if conversation.OwnerUserID != executor.UserID {
			continue
		}
		if conversation.Status != "closed" && conversation.Status != "deleted" {
			return false
		}
		activity := conversation.LastActivityAt
		if activity.IsZero() {
			activity = conversation.UpdatedAt
		}
		if activity.After(latest) {
			latest = activity
		}
	}
	return !latest.IsZero() && !latest.After(cutoff)
}
