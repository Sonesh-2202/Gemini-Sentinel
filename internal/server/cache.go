package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/sonesh-2202/gemini-sentinel/internal/gemini"
	"github.com/sonesh-2202/gemini-sentinel/internal/prompt"
	"github.com/sonesh-2202/gemini-sentinel/internal/toolbridge"
	"os"
	"path/filepath"
	"time"
)

const sessionCacheTTL = 30 * time.Minute

type persistedSessionEntry struct {
	Key  string    `json:"key"`
	Meta []any     `json:"meta"`
	At   time.Time `json:"at"`
}

func (s *Server) sessionCacheDiskPath() string {
	if s.Cfg.HomeDir == "" {
		return ""
	}
	st, err := os.Stat(s.Cfg.HomeDir)
	if err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(s.Cfg.HomeDir, "session-cache.json")
}

func (s *Server) loadSessionCacheFromDisk() {
	p := s.sessionCacheDiskPath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil || len(raw) == 0 {
		return
	}
	var entries []persistedSessionEntry
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.Key == "" || len(e.Meta) == 0 || now.Sub(e.At) >= sessionCacheTTL {
			continue
		}
		if _, exists := s.sessionCache[e.Key]; !exists {
			if len(s.sessionOrder) >= 256 {
				oldest := s.sessionOrder[0]
				s.sessionOrder = s.sessionOrder[1:]
				delete(s.sessionCache, oldest)
				delete(s.sessionCacheAt, oldest)
			}
			s.sessionOrder = append(s.sessionOrder, e.Key)
		}
		s.sessionCache[e.Key] = e.Meta
		s.sessionCacheAt[e.Key] = e.At
	}
}

func (s *Server) flushSessionCacheToDisk() {
	p := s.sessionCacheDiskPath()
	if p == "" {
		return
	}
	now := time.Now()
	s.mu.RLock()
	entries := make([]persistedSessionEntry, 0, len(s.sessionOrder))
	for _, k := range s.sessionOrder {
		meta := s.sessionCache[k]
		at := s.sessionCacheAt[k]
		if len(meta) == 0 || (!at.IsZero() && now.Sub(at) >= sessionCacheTTL) {
			continue
		}
		cp := make([]any, len(meta))
		copy(cp, meta)
		entries = append(entries, persistedSessionEntry{Key: k, Meta: cp, At: at})
	}
	s.mu.RUnlock()

	b, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, p)
	}
}

func (s *Server) getSessionCache(key string) []any {
	s.mu.RLock()
	src := s.sessionCache[key]
	at := s.sessionCacheAt[key]
	s.mu.RUnlock()
	if len(src) == 0 {
		return nil
	}
	if !at.IsZero() && time.Since(at) >= sessionCacheTTL {
		return nil
	}
	cp := make([]any, len(src))
	copy(cp, src)
	return cp
}

func (s *Server) putSessionCache(key string, meta []any) {
	if len(meta) == 0 {
		return
	}
	cp := make([]any, len(meta))
	copy(cp, meta)
	now := time.Now()
	s.mu.Lock()
	if _, exists := s.sessionCache[key]; !exists {
		if len(s.sessionOrder) >= 256 {
			oldest := s.sessionOrder[0]
			s.sessionOrder = s.sessionOrder[1:]
			delete(s.sessionCache, oldest)
			delete(s.sessionCacheAt, oldest)
		}
		s.sessionOrder = append(s.sessionOrder, key)
	}
	s.sessionCache[key] = cp
	s.sessionCacheAt[key] = now
	s.mu.Unlock()
	s.flushSessionCacheToDisk()
}

func (s *Server) saveSessionContinuation(
	accountID string,
	modelID string,
	messages []map[string]any,
	toolsRaw any,
	toolChoiceRaw any,
	content any,
	oaCalls []toolbridge.OpenAIToolCall,
	cid, rid, rcid, contextToken string,
) {
	if !s.Cfg.SessionCache || cid == "" || rid == "" || rcid == "" {
		return
	}
	var replyMsg map[string]any
	if len(oaCalls) > 0 {
		tcMaps := make([]any, 0, len(oaCalls))
		for _, tc := range oaCalls {
			tcMaps = append(tcMaps, map[string]any{
				"id":   tc.ID,
				"type": tc.Type,
				"function": map[string]any{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
		}
		replyMsg = map[string]any{
			"role":       "assistant",
			"content":    content,
			"tool_calls": tcMaps,
		}
	} else {
		replyMsg = map[string]any{
			"role":    "assistant",
			"content": content,
		}
	}
	nextHistory := make([]map[string]any, 0, len(messages)+1)
	nextHistory = append(nextHistory, messages...)
	nextHistory = append(nextHistory, replyMsg)
	nextKey := prompt.HashHistoryWithTools(accountID, modelID, nextHistory, toolsRaw, toolChoiceRaw)
	s.putSessionCache(nextKey, gemini.BuildContinuationMetadata(cid, rid, rcid, contextToken))
}

func (s *Server) uploadAttachments(
	ctx context.Context,
	acctID string,
	c UpstreamClient,
	parts []prompt.AttachmentPart,
) ([]gemini.UploadedFile, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	files := make([]gemini.UploadedFile, 0, len(parts))
	for _, att := range parts {
		uf, err := s.uploadCached(ctx, acctID, c, att)
		if err != nil {
			return nil, err
		}
		files = append(files, uf)
	}
	return files, nil
}

func (s *Server) uploadCached(ctx context.Context, acctID string, c UpstreamClient, att prompt.AttachmentPart) (gemini.UploadedFile, error) {
	if len(att.Data) == 0 && att.RemoteURL != "" {
		urlSum := sha256.Sum256([]byte(att.RemoteURL))
		urlKey := acctID + ":url:" + hex.EncodeToString(urlSum[:])
		s.mu.RLock()
		cachedURL, okURL := s.uploadCache[urlKey]
		s.mu.RUnlock()
		if okURL && time.Since(cachedURL.At) < 25*time.Minute {
			return cachedURL.File, nil
		}
		fetched, err := prompt.FetchRemoteImage(ctx, att.RemoteURL, 0)
		if err != nil {
			return gemini.UploadedFile{}, err
		}
		att = *fetched
		uf, err := s.uploadCachedBytes(ctx, acctID, c, att)
		if err != nil {
			return gemini.UploadedFile{}, err
		}
		s.putUploadCache(urlKey, uf)
		return uf, nil
	}
	return s.uploadCachedBytes(ctx, acctID, c, att)
}

func (s *Server) uploadCachedBytes(ctx context.Context, acctID string, c UpstreamClient, att prompt.AttachmentPart) (gemini.UploadedFile, error) {
	sum := sha256.Sum256(att.Data)
	cacheKey := acctID + ":" + hex.EncodeToString(sum[:])

	s.mu.RLock()
	cached, ok := s.uploadCache[cacheKey]
	s.mu.RUnlock()
	if ok && time.Since(cached.At) < 25*time.Minute {
		return cached.File, nil
	}

	uf, err := c.UploadFile(ctx, att.Data, att.Filename, att.ContentType)
	if err != nil {
		return gemini.UploadedFile{}, err
	}

	s.putUploadCache(cacheKey, uf)
	return uf, nil
}

func (s *Server) putUploadCache(cacheKey string, uf gemini.UploadedFile) {
	s.mu.Lock()
	if _, exists := s.uploadCache[cacheKey]; !exists {
		if len(s.uploadOrder) >= 128 {
			oldest := s.uploadOrder[0]
			s.uploadOrder = s.uploadOrder[1:]
			delete(s.uploadCache, oldest)
		}
		s.uploadOrder = append(s.uploadOrder, cacheKey)
	}
	s.uploadCache[cacheKey] = cachedUpload{File: uf, At: time.Now()}
	s.mu.Unlock()
}
