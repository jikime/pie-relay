package relay

import (
	"sync"
	"time"
)

// Limits bounds each public relay connection and room. Zero-valued options
// are replaced with conservative defaults by normalizeLimits.
type Limits struct {
	MaxParticipantsPerRoom int
	FramesPerSecond        float64
	FrameBurst             float64
	BytesPerSecond         float64
	ByteBurst              float64
}

func DefaultLimits() Limits {
	return Limits{
		MaxParticipantsPerRoom: 64,
		FramesPerSecond:        240,
		FrameBurst:             480,
		BytesPerSecond:         8 << 20,
		ByteBurst:              16 << 20,
	}
}

func normalizeLimits(in Limits) Limits {
	d := DefaultLimits()
	if in.MaxParticipantsPerRoom > 0 {
		d.MaxParticipantsPerRoom = in.MaxParticipantsPerRoom
	}
	if in.FramesPerSecond > 0 {
		d.FramesPerSecond = in.FramesPerSecond
	}
	if in.FrameBurst > 0 {
		d.FrameBurst = in.FrameBurst
	}
	if in.BytesPerSecond > 0 {
		d.BytesPerSecond = in.BytesPerSecond
	}
	if in.ByteBurst > 0 {
		d.ByteBurst = in.ByteBurst
	}
	return d
}

type tokenBucket struct {
	mu sync.Mutex

	frameRate   float64
	frameBurst  float64
	frameTokens float64
	byteRate    float64
	byteBurst   float64
	byteTokens  float64
	last        time.Time
}

func newTokenBucket(l Limits, now time.Time) *tokenBucket {
	return &tokenBucket{
		frameRate: l.FramesPerSecond, frameBurst: l.FrameBurst, frameTokens: l.FrameBurst,
		byteRate: l.BytesPerSecond, byteBurst: l.ByteBurst, byteTokens: l.ByteBurst,
		last: now,
	}
}

func (b *tokenBucket) Allow(bytes int, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Before(b.last) {
		now = b.last
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.frameTokens = minFloat(b.frameBurst, b.frameTokens+elapsed*b.frameRate)
	b.byteTokens = minFloat(b.byteBurst, b.byteTokens+elapsed*b.byteRate)
	if b.frameTokens < 1 || b.byteTokens < float64(bytes) {
		return false
	}
	b.frameTokens--
	b.byteTokens -= float64(bytes)
	return true
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
