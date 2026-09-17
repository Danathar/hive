package hub

// versionAbsentBeatsToConfirm is how many CONSECUTIVE version-less heartbeats
// are required before the hive is declared version-absent.
//
// One beat is not evidence. A spoke legitimately omits git_hash during
// startup, and a single collect timeout is a blip the next beat clears. A
// spoke beats roughly every 120s, so three consecutive empty beats is about
// six minutes of the hub having no version to compare - long past any
// start-up window, and short enough to have surfaced both hives above within
// minutes instead of hours.
const versionAbsentBeatsToConfirm = 3

// versionAbsentState is the per-hive run length of version-less beats. Kept
// beside the other per-beat trackers (reporterSeen, statusFlipSeen) rather
// than in the registry: it is beat bookkeeping, not hive state, and it must
// never contend with the registry lock on the hottest write path.
type versionAbsentState struct {
	// consecutive counts beats in a row that carried no git_hash. Any beat
	// WITH a version resets it to zero - a hive that recovers stops being
	// reported the moment it proves it can report again.
	consecutive int
}

// noteVersionAbsent records whether this beat carried a git_hash and returns
// true once versionAbsentBeatsToConfirm consecutive beats have carried none.
//
// gitHash is the already-sanitized, already-shortened value the registry entry
// will store, so this predicate is asking exactly the question the upgrade
// comparison asks: is there a version here to compare?
func (s *HubServer) noteVersionAbsent(hiveID, gitHash string) bool {
	s.versionAbsentMu.Lock()
	defer s.versionAbsentMu.Unlock()
	if s.versionAbsentSeen == nil {
		s.versionAbsentSeen = make(map[string]*versionAbsentState)
	}
	st := s.versionAbsentSeen[hiveID]
	if st == nil {
		st = &versionAbsentState{}
		s.versionAbsentSeen[hiveID] = st
	}
	if gitHash != "" {
		// A reported version ends the run outright. Not a decrement: the
		// signal is about an UNBROKEN streak of blindness, and one good beat
		// means the hub can compare again.
		st.consecutive = 0
		return false
	}
	// Saturate rather than counting forever, so a hive that stays mute for
	// weeks cannot overflow the counter.
	if st.consecutive < versionAbsentBeatsToConfirm {
		st.consecutive++
	}
	return st.consecutive >= versionAbsentBeatsToConfirm
}
