package session

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Session struct {
	ID           string
	UserID       int64
	WorkDir      string
	AddDirs      []string
	Created      time.Time
	MessageCount int
	Mu           sync.Mutex
}

type Manager struct {
	sessions sync.Map // map[int64]*Session
	cancels  sync.Map // map[int64]context.CancelFunc
	workDir  string
}

func NewManager(defaultWorkDir string) *Manager {
	return &Manager{workDir: defaultWorkDir}
}

func (m *Manager) Get(userID int64) *Session {
	if v, ok := m.sessions.Load(userID); ok {
		return v.(*Session)
	}
	return m.create(userID)
}

func (m *Manager) NewSession(userID int64) *Session {
	var workDir string
	var addDirs []string
	if v, ok := m.sessions.Load(userID); ok {
		old := v.(*Session)
		workDir = old.WorkDir
		addDirs = old.AddDirs
	}
	m.sessions.Delete(userID)
	s := m.create(userID)
	if workDir != "" {
		s.WorkDir = workDir
	}
	s.AddDirs = addDirs
	return s
}

func (m *Manager) SetWorkDir(userID int64, dir string) {
	s := m.Get(userID)
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.WorkDir = dir
}

func (m *Manager) AddDir(userID int64, dir string) {
	s := m.Get(userID)
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.AddDirs = append(s.AddDirs, dir)
}

func (m *Manager) IncrementMessageCount(userID int64) int {
	s := m.Get(userID)
	s.MessageCount++
	return s.MessageCount
}

// SetCancel stores a cancel function for the user's current request.
func (m *Manager) SetCancel(userID int64, cancel context.CancelFunc) {
	m.cancels.Store(userID, cancel)
}

// ClearCancel removes the stored cancel function.
func (m *Manager) ClearCancel(userID int64) {
	m.cancels.Delete(userID)
}

// Cancel calls and removes the stored cancel function, returning true if one existed.
func (m *Manager) Cancel(userID int64) bool {
	if v, ok := m.cancels.LoadAndDelete(userID); ok {
		v.(context.CancelFunc)()
		return true
	}
	return false
}

func (m *Manager) create(userID int64) *Session {
	s := &Session{
		ID:      uuid.New().String(),
		UserID:  userID,
		WorkDir: m.workDir,
		Created: time.Now(),
	}
	actual, _ := m.sessions.LoadOrStore(userID, s)
	return actual.(*Session)
}
