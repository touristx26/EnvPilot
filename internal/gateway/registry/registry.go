// Package registry 维护已注册环境的元数据与在线状态。
// 内存为运行时状态，SQLite (store) 为持久层：启动时加载，变更时写穿透。
package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sync"
	"time"

	"envpilot/internal/gateway/store"
	"envpilot/internal/proto"
)

// Environment 描述一个已注册的环境。
type Environment struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	Tags         map[string]string `json:"tags,omitempty"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Online       bool              `json:"online"`
	LastSeen     time.Time         `json:"last_seen"`
	RegisteredAt time.Time         `json:"registered_at"`
}

// Registry 环境注册表（并发安全）。
type Registry struct {
	mu    sync.RWMutex
	envs  map[string]*Environment
	names map[string]string // name → envID，重连时复用 ID
	st    *store.Store      // 可为 nil（无持久化模式，便于测试）
}

// New 创建注册表。st 非 nil 时从持久层加载历史环境（均置为离线，等待 agent 重连）。
func New(st *store.Store) *Registry {
	r := &Registry{
		envs:  make(map[string]*Environment),
		names: make(map[string]string),
		st:    st,
	}
	if st != nil {
		r.loadFromStore()
	}
	return r
}

func (r *Registry) loadFromStore() {
	recs, err := r.st.ListEnvironments()
	if err != nil {
		log.Printf("[registry] failed to load environments from store: %v", err)
		return
	}
	for _, rec := range recs {
		env := &Environment{
			ID:           rec.ID,
			Name:         rec.Name,
			Type:         rec.Type,
			OS:           rec.OS,
			Arch:         rec.Arch,
			Online:       false, // 网关刚启动，等待 agent 重连后置在线
			LastSeen:     rec.LastSeen,
			RegisteredAt: rec.RegisteredAt,
		}
		_ = json.Unmarshal([]byte(rec.Tags), &env.Tags)
		_ = json.Unmarshal([]byte(rec.Capabilities), &env.Capabilities)
		r.envs[env.ID] = env
		r.names[env.Name] = env.ID
	}
	if len(recs) > 0 {
		log.Printf("[registry] loaded %d environments from store", len(recs))
	}
}

// Register 注册或更新环境（同名环境重连时复用原 ID），返回环境 ID。
func (r *Registry) Register(req proto.RegisterRequest) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	var env *Environment
	if id, ok := r.names[req.Name]; ok {
		if e, ok := r.envs[id]; ok {
			env = e
			env.Type = req.Type
			env.Tags = req.Tags
			env.OS = req.OS
			env.Arch = req.Arch
			env.Capabilities = req.Capabilities
			env.Online = true
			env.LastSeen = now
		}
	}
	if env == nil {
		env = &Environment{
			ID:           newEnvID(r),
			Name:         req.Name,
			Type:         req.Type,
			Tags:         req.Tags,
			OS:           req.OS,
			Arch:         req.Arch,
			Capabilities: req.Capabilities,
			Online:       true,
			LastSeen:     now,
			RegisteredAt: now,
		}
		r.envs[env.ID] = env
		r.names[req.Name] = env.ID
	}

	// 写穿透持久层（尽力而为，失败不阻断注册）
	if r.st != nil {
		tags, _ := json.Marshal(env.Tags)
		caps, _ := json.Marshal(env.Capabilities)
		if err := r.st.SaveEnvironment(store.EnvRecord{
			ID:           env.ID,
			Name:         env.Name,
			Type:         env.Type,
			Tags:         string(tags),
			OS:           env.OS,
			Arch:         env.Arch,
			Capabilities: string(caps),
			RegisteredAt: env.RegisteredAt,
			LastSeen:     env.LastSeen,
		}); err != nil {
			log.Printf("[registry] failed to persist environment %s: %v", env.ID, err)
		}
	}
	return env.ID
}

// MarkSeen 刷新环境心跳时间（仅内存，落库由离线/注册时机承担）。
func (r *Registry) MarkSeen(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if env, ok := r.envs[id]; ok {
		env.LastSeen = time.Now()
		env.Online = true
	}
}

// SetOffline 将环境置为离线并持久化。
func (r *Registry) SetOffline(id string) {
	r.mu.Lock()
	env, ok := r.envs[id]
	if ok {
		env.Online = false
	}
	r.mu.Unlock()
	if ok && r.st != nil {
		if err := r.st.SetEnvironmentOnline(id, false); err != nil {
			log.Printf("[registry] failed to persist offline state for %s: %v", id, err)
		}
	}
}

// Get 返回环境副本。
func (r *Registry) Get(id string) (Environment, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	env, ok := r.envs[id]
	if !ok {
		return Environment{}, false
	}
	return *env, true
}

// List 返回所有环境的副本。
func (r *Registry) List() []Environment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Environment, 0, len(r.envs))
	for _, env := range r.envs {
		out = append(out, *env)
	}
	return out
}

// StartSweeper 启动后台协程，将超过 ttl 未心跳的环境置为离线。
func (r *Registry) StartSweeper(ctx context.Context, interval, ttl time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				deadline := time.Now().Add(-ttl)
				r.mu.RLock()
				var timedOut []string
				for id, env := range r.envs {
					if env.Online && env.LastSeen.Before(deadline) {
						timedOut = append(timedOut, id)
					}
				}
				r.mu.RUnlock()
				for _, id := range timedOut {
					r.SetOffline(id)
					if env, ok := r.Get(id); ok {
						log.Printf("[registry] environment %s (%s) marked offline: heartbeat timeout", id, env.Name)
					}
				}
			}
		}
	}()
}

func newEnvID(r *Registry) string {
	gen := func() string {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return time.Now().Format("01021504.05.000000000") // 降级方案
		}
		return hex.EncodeToString(b)
	}
	id := "env-" + gen()
	for _, exists := r.envs[id]; exists; {
		id = "env-" + gen()
	}
	return id
}
