// Package middleware provides snowplow-local HTTP middleware mounted on the
// server mux. Its UserConfig middleware is a transcription of plumbing's
// use.UserConfig that resolves the per-request user client-config, adding a
// cache-first lookup against the in-process Secrets snapshot before falling
// back to the apiserver GET (the fallback path is byte-identical to
// upstream). It lives here because the upstream module cannot be patched.
package middleware
