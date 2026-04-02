package pull

// hls.go — HLS pull ingestor.
//
// Downloads and parses M3U8 playlists without any third-party parser library.
// Supports live (sliding window), VOD (#EXT-X-ENDLIST), and adaptive master
// playlists — the highest-bandwidth variant is selected automatically.
//
// Data flow:
//
//	Open() → poll goroutine → fetchPlaylistWithRetry → fetchSegmentWithRetry
//	                        ↓ one hlsResult per segment
//	Read() ← out channel (buffered; closed when the source is exhausted)
//
// Reliability contract:
//   - Transient segment failures: logged and skipped; streaming continues.
//   - Persistent playlist failures: terminal error forwarded to Read callers.
//   - VOD (#EXT-X-ENDLIST): io.EOF delivered after all segments are consumed.
//   - Deduplication by media-sequence number: O(1) state, no map growth.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ntthuan060102github/open-streamer/config"
	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

const (
	hlsDefaultPlaylistTimeout = 15 * time.Second
	hlsDefaultSegmentTimeout  = 60 * time.Second // segments can be many MB
	hlsMinPollInterval        = 500 * time.Millisecond
	hlsMaxPlaylistRetries     = 5
	hlsMaxSegmentRetries      = 2
	hlsRetryBase              = 500 * time.Millisecond
)

// M3U8 tag constants — used in both the parser and the media-tag detector.
const (
	m3u8TagMediaSeq   = "#EXT-X-MEDIA-SEQUENCE:"
	m3u8TagTargetDur  = "#EXT-X-TARGETDURATION:"
	m3u8TagEndList    = "#EXT-X-ENDLIST"
	m3u8TagDisconuity = "#EXT-X-DISCONTINUITY"
	m3u8TagExtInf     = "#EXTINF:"
	m3u8TagStreamInf  = "#EXT-X-STREAM-INF:"
)

// ─── Result channel type ────────────────────────────────────────────────────

type hlsResult struct {
	data []byte
	err  error // non-nil only on terminal condition (io.EOF or fatal error)
}

// ─── M3U8 types ─────────────────────────────────────────────────────────────

type hlsMediaPlaylist struct {
	seqBase        uint64
	targetDuration float64
	ended          bool
	segments       []hlsSegment
}

type hlsSegment struct {
	uri          string
	seq          uint64
	duration     float64
	discontinuity bool
}

type hlsVariant struct {
	uri       string
	bandwidth uint32
}

// ─── HLSReader ───────────────────────────────────────────────────────────────

// HLSReader polls an HLS playlist and emits MPEG-TS segment bytes in order.
// It implements TSChunkReader and is intended to wrap with NewTSDemuxPacketReader.
//
// Expected URL format: http[s]://host/path/to/playlist.m3u8
// Master playlists are transparently resolved to the highest-bandwidth variant.
type HLSReader struct {
	input domain.Input
	cfg   config.IngestorConfig

	// plClient: short-timeout client for playlist GETs.
	// segClient: long-timeout client for segment body reads.
	plClient  *http.Client
	segClient *http.Client

	out    chan hlsResult
	cancel context.CancelFunc
	once   sync.Once
}

// NewHLSReader constructs an HLSReader without opening the connection.
func NewHLSReader(input domain.Input, cfg config.IngestorConfig) *HLSReader {
	maxBuf := cfg.HLSMaxSegmentBuffer
	if maxBuf <= 0 {
		maxBuf = 8
	}
	return &HLSReader{
		input: input,
		cfg:   cfg,
		out:   make(chan hlsResult, maxBuf),
	}
}

// Open starts the background polling goroutine.
// Returns immediately; segments arrive asynchronously via Read.
func (r *HLSReader) Open(ctx context.Context) error {
	plTimeout := time.Duration(r.input.Net.ConnectTimeoutSec) * time.Second
	if plTimeout == 0 {
		plTimeout = hlsDefaultPlaylistTimeout
	}
	segTimeout := plTimeout * 4
	if segTimeout < hlsDefaultSegmentTimeout {
		segTimeout = hlsDefaultSegmentTimeout
	}

	r.plClient = &http.Client{Timeout: plTimeout}
	r.segClient = &http.Client{Timeout: segTimeout}

	pollCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	go r.poll(pollCtx, r.input.URL)
	return nil
}

// Read blocks until the next MPEG-TS segment chunk is available.
// Returns (nil, io.EOF) when a VOD stream is fully consumed or the source ends.
func (r *HLSReader) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res, ok := <-r.out:
		if !ok {
			return nil, io.EOF
		}
		return res.data, res.err
	}
}

// Close stops the poll goroutine. Safe to call before Open or more than once.
func (r *HLSReader) Close() error {
	r.once.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
	})
	return nil
}

// ─── Poll loop ───────────────────────────────────────────────────────────────

func (r *HLSReader) poll(ctx context.Context, rawURL string) {
	defer close(r.out)

	mediaURL := rawURL
	var started bool
	var lastSeq uint64

	for {
		if ctx.Err() != nil {
			return
		}

		pl, err := r.fetchPlaylistWithRetry(ctx, &mediaURL)
		if err != nil {
			r.sendResult(ctx, hlsResult{err: err})
			return
		}

		delivered := r.deliverNewSegments(ctx, pl, &started, &lastSeq)
		if !delivered {
			return // context cancelled inside deliverNewSegments
		}

		if pl.ended {
			r.sendResult(ctx, hlsResult{err: io.EOF})
			return
		}

		if !r.waitForNextPoll(ctx, pl.targetDuration) {
			return
		}
	}
}

// deliverNewSegments downloads and queues segments not yet delivered.
// Returns false when the context was cancelled.
func (r *HLSReader) deliverNewSegments(
	ctx context.Context,
	pl *hlsMediaPlaylist,
	started *bool,
	lastSeq *uint64,
) bool {
	for _, seg := range pl.segments {
		if *started && seg.seq <= *lastSeq {
			continue
		}
		data, err := r.fetchSegmentWithRetry(ctx, seg.uri)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			slog.Warn("hls: segment fetch failed, skipping",
				"url", seg.uri, "err", err)
			continue
		}
		if !r.sendResult(ctx, hlsResult{data: data}) {
			return false
		}
		*lastSeq = seg.seq
		*started = true
	}
	return true
}

// sendResult sends a result to the out channel, returning false if cancelled.
func (r *HLSReader) sendResult(ctx context.Context, res hlsResult) bool {
	select {
	case r.out <- res:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitForNextPoll waits half a target duration before the next playlist poll.
// Returns false when the context is cancelled.
func (r *HLSReader) waitForNextPoll(ctx context.Context, targetDuration float64) bool {
	if targetDuration <= 0 {
		targetDuration = 2
	}
	wait := time.Duration(float64(time.Second) * targetDuration / 2)
	if wait < hlsMinPollInterval {
		wait = hlsMinPollInterval
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ─── Playlist fetching ───────────────────────────────────────────────────────

// fetchPlaylistWithRetry fetches and parses the playlist, resolving a master
// playlist to its highest-bandwidth variant. Retries transient HTTP errors.
// The mediaURL pointer is updated when a master → media redirect occurs.
func (r *HLSReader) fetchPlaylistWithRetry(ctx context.Context, mediaURL *string) (*hlsMediaPlaylist, error) {
	var lastErr error
	for attempt := 0; attempt <= hlsMaxPlaylistRetries; attempt++ {
		if attempt > 0 {
			if !r.retryWait(ctx, attempt-1) {
				return nil, ctx.Err()
			}
		}
		pl, err := r.resolveAndFetch(ctx, mediaURL)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			slog.Warn("hls: playlist fetch failed, retrying",
				"url", *mediaURL, "attempt", attempt+1, "err", err)
			continue
		}
		return pl, nil
	}
	return nil, fmt.Errorf("hls: playlist %q: %w (after %d retries)", *mediaURL, lastErr, hlsMaxPlaylistRetries)
}

// resolveAndFetch fetches the URL; if it is a master playlist, selects the
// best variant, updates *mediaURL, and fetches again (one level of redirection).
func (r *HLSReader) resolveAndFetch(ctx context.Context, mediaURL *string) (*hlsMediaPlaylist, error) {
	for {
		result, err := r.fetchPlaylistOnce(ctx, *mediaURL)
		if err != nil {
			return nil, err
		}
		if !result.isMaster() {
			return result.media, nil
		}
		best := pickBestVariant(result.variants)
		if best == "" {
			return nil, fmt.Errorf("hls: master playlist has no usable variants")
		}
		*mediaURL = best
	}
}

type fetchResult struct {
	media    *hlsMediaPlaylist
	variants []hlsVariant
}

func (f *fetchResult) isMaster() bool { return f.media == nil }

// fetchPlaylistOnce performs a single HTTP GET and parses the M3U8 body.
func (r *HLSReader) fetchPlaylistOnce(ctx context.Context, playlistURL string) (*fetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playlistURL, nil)
	if err != nil {
		return nil, fmt.Errorf("hls: build request: %w", err)
	}
	for k, v := range r.input.Headers {
		req.Header.Set(k, v)
	}

	resp, err := r.plClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hls: GET %q: %w", playlistURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hls: GET %q: HTTP %d", playlistURL, resp.StatusCode)
	}

	base, _ := url.Parse(playlistURL)
	return parseM3U8(resp.Body, base)
}

// ─── Segment fetching ────────────────────────────────────────────────────────

// fetchSegmentWithRetry downloads a segment, retrying transient failures.
func (r *HLSReader) fetchSegmentWithRetry(ctx context.Context, segURL string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= hlsMaxSegmentRetries; attempt++ {
		if attempt > 0 {
			if !r.retryWait(ctx, attempt-1) {
				return nil, ctx.Err()
			}
		}
		data, err := r.fetchSegmentOnce(ctx, segURL)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

func (r *HLSReader) fetchSegmentOnce(ctx context.Context, segURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range r.input.Headers {
		req.Header.Set(k, v)
	}
	resp, err := r.segClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hls: GET segment %q: %w", segURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hls: segment %q: HTTP %d", segURL, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// retryWait waits for exponential backoff (hlsRetryBase × 2^attempt).
// Returns false when the context is cancelled.
func (r *HLSReader) retryWait(ctx context.Context, attempt int) bool {
	delay := hlsRetryBase * (1 << attempt)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ─── M3U8 parser ────────────────────────────────────────────────────────────

// parseM3U8 reads an M3U8 playlist from r and returns either a media playlist
// or a list of master variants. Base is used to resolve relative URIs.
func parseM3U8(body io.Reader, base *url.URL) (*fetchResult, error) {
	var (
		segments    []hlsSegment
		variants    []hlsVariant
		seqBase     uint64
		targetDur   float64
		ended       bool
		isMediaPL   bool
		pendingDur  float64
		pendingDisc bool
	)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "#EXTM3U" {
			continue
		}
		isMediaPL = isMediaPL || isMediaTag(line)
		if err := parseLine(line, base, scanner, &parseState{
			seqBase:     &seqBase,
			targetDur:   &targetDur,
			ended:       &ended,
			pendingDur:  &pendingDur,
			pendingDisc: &pendingDisc,
			segments:    &segments,
			variants:    &variants,
		}); err != nil {
			return nil, err
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("hls: m3u8 scan: %w", err)
	}

	if !isMediaPL && len(variants) > 0 {
		return &fetchResult{variants: variants}, nil
	}

	return &fetchResult{media: &hlsMediaPlaylist{
		seqBase:        seqBase,
		targetDuration: targetDur,
		ended:          ended,
		segments:       segments,
	}}, nil
}

type parseState struct {
	seqBase     *uint64
	targetDur   *float64
	ended       *bool
	pendingDur  *float64
	pendingDisc *bool
	segments    *[]hlsSegment
	variants    *[]hlsVariant
}

// parseLine processes one M3U8 tag or URI line, updating parseState.
func parseLine(line string, base *url.URL, scanner *bufio.Scanner, st *parseState) error {
	switch {
	case strings.HasPrefix(line, m3u8TagMediaSeq):
		n, _ := strconv.ParseUint(strings.TrimPrefix(line, m3u8TagMediaSeq), 10, 64)
		*st.seqBase = n

	case strings.HasPrefix(line, m3u8TagTargetDur):
		d, _ := strconv.ParseFloat(strings.TrimPrefix(line, m3u8TagTargetDur), 64)
		*st.targetDur = d

	case line == m3u8TagEndList:
		*st.ended = true

	case line == m3u8TagDisconuity:
		*st.pendingDisc = true

	case strings.HasPrefix(line, m3u8TagExtInf):
		*st.pendingDur = parseINFDuration(strings.TrimPrefix(line, m3u8TagExtInf))

	case strings.HasPrefix(line, m3u8TagStreamInf):
		bw := parseBandwidthAttr(strings.TrimPrefix(line, m3u8TagStreamInf))
		if scanner.Scan() {
			uri := strings.TrimSpace(scanner.Text())
			if uri != "" && !strings.HasPrefix(uri, "#") {
				*st.variants = append(*st.variants, hlsVariant{
					uri:       resolveHLSURL(base, uri),
					bandwidth: bw,
				})
			}
		}

	case !strings.HasPrefix(line, "#") && *st.pendingDur > 0:
		seqNo := *st.seqBase + uint64(len(*st.segments))
		*st.segments = append(*st.segments, hlsSegment{
			uri:          resolveHLSURL(base, line),
			seq:          seqNo,
			duration:     *st.pendingDur,
			discontinuity: *st.pendingDisc,
		})
		*st.pendingDur = 0
		*st.pendingDisc = false
	}
	return nil
}

// isMediaTag returns true if the line signals a media (not master) playlist.
func isMediaTag(line string) bool {
	return strings.HasPrefix(line, m3u8TagTargetDur) ||
		strings.HasPrefix(line, m3u8TagMediaSeq) ||
		strings.HasPrefix(line, m3u8TagExtInf) ||
		line == m3u8TagEndList
}

// parseINFDuration extracts the float duration from the #EXTINF value string.
// Format: "duration,title" or "duration".
func parseINFDuration(val string) float64 {
	if idx := strings.IndexByte(val, ','); idx >= 0 {
		val = val[:idx]
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(val), 64)
	return d
}

// parseBandwidthAttr extracts the BANDWIDTH= integer from an attribute list.
func parseBandwidthAttr(attrs string) uint32 {
	for _, part := range strings.Split(attrs, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToUpper(part), "BANDWIDTH=") {
			val := strings.SplitN(part, "=", 2)
			if len(val) == 2 {
				n, _ := strconv.ParseUint(strings.TrimSpace(val[1]), 10, 32)
				return uint32(n)
			}
		}
	}
	return 0
}

// pickBestVariant returns the URI of the variant with the highest bandwidth.
func pickBestVariant(variants []hlsVariant) string {
	var best string
	var bestBW uint32
	for _, v := range variants {
		if v.bandwidth >= bestBW {
			bestBW = v.bandwidth
			best = v.uri
		}
	}
	return best
}

// resolveHLSURL resolves ref against base, returning an absolute URL string.
func resolveHLSURL(base *url.URL, ref string) string {
	if base == nil {
		return ref
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(refURL).String()
}
