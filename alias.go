// alias.go — tenant domain aliases.
//
// A tenant is known by exactly one ref, its domain, and that string is its
// identity everywhere: the database file, the ref column on every row, the
// realtime channel names, the session cookie. A vendor with two domains
// (example.com and account.example.com) would therefore be two tenants,
// with a visitor on one never reaching the agents signed in to the other.
//
// An alias fixes that at the door rather than in each of those places. It is
// looked up FIRST, before a ref is used for anything, and swapped for the ref
// it points at; from there on the request is indistinguishable from one that
// named the real domain. Nothing downstream knows aliases exist, which is what
// keeps the database, the rows and the channels in agreement with each other.
//
// Resolving any later — say, only when picking the database file — would put
// both domains in one database while their rows and channels still carried
// different refs, so guests and agents would share storage and still miss each
// other.
package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// refAliases maps an alias to the ref it points at. Keys are in safeRefFile
// form, so every spelling that would have opened the alias's own database
// (mixed case, a trailing dot) resolves the same way.
//
// Written once at startup by loadRefAliases and only read afterwards, so it
// needs no lock.
var refAliases map[string]string

// parseRefAliases reads REF_ALIASES: comma-separated alias=domain pairs.
//
//	REF_ALIASES=account.example.com=example.com,shop.acme.com=acme.com
//
// A bad entry is logged and skipped rather than refusing to start: one typo in
// one vendor's alias must not take every other tenant offline with it.
func parseRefAliases(spec string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		alias, target, ok := strings.Cut(pair, "=")
		a := safeRefFile(strings.TrimSpace(alias))
		t := safeRefFile(strings.TrimSpace(target))
		if !ok || a == "" || t == "" {
			log.Printf("[Alias] ignoring %q: expected alias=domain", pair)
			continue
		}
		if a == t {
			log.Printf("[Alias] ignoring %q: a domain cannot be an alias of itself", pair)
			continue
		}
		out[a] = t
	}
	// One hop only. An alias pointing at another alias is either a chain, where
	// the answer would depend on how many times we looked, or a loop, where
	// there is no answer. Collected first and removed after, so the outcome
	// does not depend on map iteration order.
	var chained []string
	for a, t := range out {
		if _, isAlias := out[t]; isAlias {
			chained = append(chained, a)
		}
	}
	for _, a := range chained {
		log.Printf("[Alias] ignoring %s=%s: %s is itself an alias; point it at the real domain", a, out[a], out[a])
		delete(out, a)
	}
	return out
}

// loadRefAliases installs the alias table and reports what it will do.
//
// An alias that already has a database of its own is the case that needs a
// human. From now on that name resolves elsewhere, so its database is no longer
// served — not deleted, not merged, just unreachable — and whether its history
// should be merged into the real tenant is not something to guess at. Said
// loudly here, at startup, because nothing at request time would ever mention
// it.
func loadRefAliases(spec, dataDir string) {
	refAliases = parseRefAliases(spec)
	for alias, target := range refAliases {
		log.Printf("[Alias] %s is an alias of %s", alias, target)
		if _, err := os.Stat(filepath.Join(dataDir, alias+".db")); err == nil {
			log.Printf("[Alias] WARNING: %s.db exists, but %s now resolves to %s — "+
				"that database is no longer served. Merge it into %s.db or move it aside.",
				alias, alias, target, target)
		}
	}
}

// isRefAlias reports whether a safeRefFile-form key is a configured alias.
func isRefAlias(key string) bool {
	_, ok := refAliases[key]
	return ok
}

// resolveRef returns the ref a request should proceed with: the alias's target
// if ref is an alias, otherwise ref exactly as given.
//
// Exactly as given, including its case, when it is not an alias. Rows and
// channels are keyed by the ref as it arrived, so normalising here would
// quietly orphan whatever an existing tenant stored under a different
// spelling. Aliases are the only thing this changes.
func resolveRef(ref string) string {
	if len(refAliases) == 0 {
		return ref
	}
	if target, ok := refAliases[safeRefFile(strings.TrimSpace(ref))]; ok {
		return target
	}
	return ref
}

// resolveChannel applies resolveRef to the ref inside a realtime channel name.
//
// The WebSocket is the one place a ref arrives embedded in another string
// instead of as a field of its own. Only the namespaces keyed by ref are
// touched; conv:<cid> and guest:<sid> are keyed by random ids and pass through.
func resolveChannel(channel string) string {
	if len(refAliases) == 0 {
		return channel
	}
	for _, ns := range []string{"presence:", "dashboard:"} {
		if ref, ok := strings.CutPrefix(channel, ns); ok {
			return ns + resolveRef(ref)
		}
	}
	// inbox:user:<ref>:<userID> — the ref is everything up to the last colon.
	if rest, ok := strings.CutPrefix(channel, "inbox:user:"); ok {
		if i := strings.LastIndex(rest, ":"); i > 0 {
			return "inbox:user:" + resolveRef(rest[:i]) + rest[i:]
		}
	}
	return channel
}

// resolveAliases wraps the whole server so a ?ref= naming an alias is rewritten
// before any handler runs.
//
// Done here, once, rather than at each of the handlers that read the query
// string: a handler added later cannot forget a step it never had to take.
// Refs that arrive some other way are resolved where they are decoded — JSON
// and form bodies, the session cookie, platform tokens, channel names.
func resolveAliases(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(refAliases) == 0 || r.URL.RawQuery == "" {
			next.ServeHTTP(w, r)
			return
		}
		q := r.URL.Query()
		refs, changed := q["ref"], false
		for i, v := range refs {
			if resolved := resolveRef(v); resolved != v {
				refs[i], changed = resolved, true
			}
		}
		if !changed {
			next.ServeHTTP(w, r)
			return
		}
		// A copy, as http.StripPrefix does: the incoming request is not ours to
		// edit in place.
		r2 := new(http.Request)
		*r2 = *r
		r2.URL = new(url.URL)
		*r2.URL = *r.URL
		r2.URL.RawQuery = q.Encode()
		next.ServeHTTP(w, r2)
	})
}
