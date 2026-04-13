package coordinator

import (
	"reflect"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

// StreamDiff describes what changed between two versions of a stream configuration.
// Only non-metadata fields are compared (name, description, tags, timestamps are ignored).
type StreamDiff struct {
	// Inputs
	InputsChanged bool
	AddedInputs   []domain.Input // priority exists in new but not old
	RemovedInputs []domain.Input // priority exists in old but not new
	UpdatedInputs []domain.Input // same priority, different config (carries NEW input)

	// Transcoder
	TranscoderChanged         bool          // any transcoder config change
	TranscoderTopologyChanged bool          // nil↔non-nil, mode change, video.copy change → buffer layout changes
	ProfilesDiff              *ProfilesDiff // per-profile granular diff; nil when topology changed

	// Publisher
	ProtocolsChanged bool
	PushChanged      bool

	// DVR
	DVRChanged bool

	// Lifecycle
	NowDisabled bool // was enabled → now disabled
}

// ProfilesDiff holds per-profile changes when the transcoder topology stays the same.
type ProfilesDiff struct {
	Added   []ProfileChange
	Removed []ProfileChange
	Updated []ProfileChange
}

// ProfileChange describes one profile that was added, removed, or updated.
type ProfileChange struct {
	Index int
	Old   *domain.VideoProfile // nil when added
	New   *domain.VideoProfile // nil when removed
}

// HasProfileChanges reports whether there are any per-profile changes.
func (d *ProfilesDiff) HasProfileChanges() bool {
	return d != nil && (len(d.Added) > 0 || len(d.Removed) > 0 || len(d.Updated) > 0)
}

// HasAddedOrRemoved reports whether the profile count changed (master playlist rebuild needed).
func (d *ProfilesDiff) HasAddedOrRemoved() bool {
	return d != nil && (len(d.Added) > 0 || len(d.Removed) > 0)
}

// ComputeDiff compares two stream configurations and returns a diff.
func ComputeDiff(old, new *domain.Stream) StreamDiff {
	var d StreamDiff

	d.NowDisabled = !old.Disabled && new.Disabled

	diffInputs(old, new, &d)
	diffTranscoder(old, new, &d)
	d.ProtocolsChanged = old.Protocols != new.Protocols
	d.PushChanged = !pushSliceEqual(old.Push, new.Push)
	d.DVRChanged = !dvrConfigEqual(old.DVR, new.DVR)

	return d
}

// diffInputs compares inputs by priority as the stable key.
func diffInputs(old, new *domain.Stream, d *StreamDiff) {
	oldMap := make(map[int]domain.Input, len(old.Inputs))
	for _, inp := range old.Inputs {
		oldMap[inp.Priority] = inp
	}
	newMap := make(map[int]domain.Input, len(new.Inputs))
	for _, inp := range new.Inputs {
		newMap[inp.Priority] = inp
	}

	for p, ni := range newMap {
		oi, exists := oldMap[p]
		if !exists {
			d.AddedInputs = append(d.AddedInputs, ni)
			continue
		}
		if !inputsEqual(oi, ni) {
			d.UpdatedInputs = append(d.UpdatedInputs, ni)
		}
	}
	for p, oi := range oldMap {
		if _, exists := newMap[p]; !exists {
			d.RemovedInputs = append(d.RemovedInputs, oi)
		}
	}

	d.InputsChanged = len(d.AddedInputs) > 0 || len(d.RemovedInputs) > 0 || len(d.UpdatedInputs) > 0
}

// diffTranscoder compares transcoder configs with two granularity levels:
// 1. Topology change (buffer layout changes) → full restart needed
// 2. Per-profile diff (only specific FFmpeg processes need restart)
func diffTranscoder(old, new *domain.Stream, d *StreamDiff) {
	if reflect.DeepEqual(old.Transcoder, new.Transcoder) {
		return
	}
	d.TranscoderChanged = true

	// nil ↔ non-nil is a topology change.
	if (old.Transcoder == nil) != (new.Transcoder == nil) {
		d.TranscoderTopologyChanged = true
		return
	}

	// Both non-nil from here.
	ot, nt := old.Transcoder, new.Transcoder

	// Mode change is a topology change.
	if effectiveMode(ot.Mode) != effectiveMode(nt.Mode) {
		d.TranscoderTopologyChanged = true
		return
	}

	// video.copy change is a topology change (switches between raw buffer and main buffer).
	if ot.Video.Copy != nt.Video.Copy {
		d.TranscoderTopologyChanged = true
		return
	}

	// If mode is passthrough/remux, there are no profiles to diff.
	if effectiveMode(nt.Mode) != domain.TranscodeModeFull {
		d.TranscoderTopologyChanged = true
		return
	}

	// Global, audio, decoder, or extra_args change → all profiles must restart.
	globalChanged := ot.Global != nt.Global ||
		!reflect.DeepEqual(ot.Audio, nt.Audio) ||
		ot.Decoder != nt.Decoder ||
		!reflect.DeepEqual(ot.ExtraArgs, nt.ExtraArgs)

	pd := diffProfiles(ot.Video.Profiles, nt.Video.Profiles, globalChanged)
	d.ProfilesDiff = pd
}

// diffProfiles compares video profile slices by index.
func diffProfiles(oldProfiles, newProfiles []domain.VideoProfile, allChanged bool) *ProfilesDiff {
	pd := &ProfilesDiff{}

	maxLen := len(oldProfiles)
	if len(newProfiles) > maxLen {
		maxLen = len(newProfiles)
	}

	for i := range maxLen {
		haveOld := i < len(oldProfiles)
		haveNew := i < len(newProfiles)

		switch {
		case haveOld && haveNew:
			if allChanged || oldProfiles[i] != newProfiles[i] {
				op := oldProfiles[i]
				np := newProfiles[i]
				pd.Updated = append(pd.Updated, ProfileChange{Index: i, Old: &op, New: &np})
			}
		case haveOld && !haveNew:
			op := oldProfiles[i]
			pd.Removed = append(pd.Removed, ProfileChange{Index: i, Old: &op})
		case !haveOld && haveNew:
			np := newProfiles[i]
			pd.Added = append(pd.Added, ProfileChange{Index: i, New: &np})
		}
	}

	return pd
}

func effectiveMode(m domain.TranscodeMode) domain.TranscodeMode {
	if m == "" {
		return domain.TranscodeModeFull
	}
	return m
}

func inputsEqual(a, b domain.Input) bool {
	return a.URL == b.URL &&
		reflect.DeepEqual(a.Headers, b.Headers) &&
		reflect.DeepEqual(a.Params, b.Params) &&
		a.Net == b.Net
}

func pushSliceEqual(a, b []domain.PushDestination) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].URL != b[i].URL ||
			a[i].Enabled != b[i].Enabled ||
			a[i].TimeoutSec != b[i].TimeoutSec ||
			a[i].RetryTimeoutSec != b[i].RetryTimeoutSec ||
			a[i].Limit != b[i].Limit {
			return false
		}
	}
	return true
}

func dvrConfigEqual(a, b *domain.StreamDVRConfig) bool {
	if a == nil && b == nil {
		return true
	}
	if (a == nil) != (b == nil) {
		return false
	}
	return *a == *b
}
