package dashboard

// liveSessionRole resolves the CURRENT role for a session-authenticated user,
// consulting this hive's authorized-users allowlist on EVERY request rather
// than trusting the role frozen into the session at login.
//
//   - If the allowlist has an entry for the user (canonical or legacy identity
//     form — see config.AuthorizedRole), that entry's role is authoritative:
//     upgrades AND downgrades granted hub-side take effect immediately.
//   - If the allowlist is enforced (direct-route spoke) and the user is absent,
//     their access was revoked: returns ok=false and the caller must treat the
//     session as unauthenticated. A revoked user must not coast on a stale
//     session for up to 30 days.
//   - If no allowlist governs this spoke (hub-proxied or open/dev), the
//     session's own role stands — the hub nginx or the deployment's open trust
//     model is the authority there, not a list this spoke doesn't have.
func (s *Server) liveSessionRole(sess *userSession) (role string, ok bool) {
	if sess == nil {
		return "", false
	}
	return s.liveAllowlistRole(sess.Username, sess.Role)
}

// liveAllowlistRole is the single shared rule for resolving a user's CURRENT
// role on this spoke: the allowlist entry when one exists, the caller's
// fallback when no allowlist governs, and ok=false (revoked — treat as
// unauthenticated/denied) when the allowlist is enforced and the user is
// absent. Session injection (authenticate), the SSO handoff mint, and terminal
// assertion renewal all resolve through here so they can never disagree.
func (s *Server) liveAllowlistRole(username, fallback string) (role string, ok bool) {
	role = fallback
	if s.deps != nil && s.deps.Config != nil {
		if allowRole, found := s.deps.Config.Dashboard.AuthorizedRole(username); found {
			role = allowRole
		} else if s.deps.Config.Dashboard.IsDirectRouteAuthzEnabled() {
			return "", false
		}
	}
	return role, true
}
