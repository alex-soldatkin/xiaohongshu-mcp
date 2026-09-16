package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-rod/rod"
)

// stateReaderJS walks a dotted path through window.__INITIAL_STATE__ and
// returns the value at the end of it as JSON, or "" if the path does not
// resolve. It is the only snippet in the package that touches page state:
// every reader goes through readStateJSON below.
//
// Why a helper at all. The site's state is a live Vue reactive store, so any
// hop along the path may be a ref rather than the value itself, and which hops
// are refs differs between deployments (and between releases of the same
// deployment). Readers that index straight through get `undefined` — a
// successful call that quietly finds nothing, which is the worst failure mode
// available. Unwrapping at every hop removes the guesswork.
//
// Two ref shapes exist and both are accepted:
//
//   - live: a Vue RefImpl, with a `.value` getter, `__v_isRef === true`, and a
//     `dep` holding the subscriber list;
//   - serialized: the SSR payload, a plain object carrying `_rawValue` and
//     `_value` and no getter.
//
// `.value` is preferred because a live ref's `_value` may be a reactive Proxy;
// `_rawValue` is the fallback for the serialized form.
//
// The replacer is not a nicety. A live ref's `dep` holds a circular subscriber
// list, so JSON.stringify over a subtree containing one throws "Converting
// circular structure to JSON". Replacing each ref with its unwrapped value
// before stringify descends into it means `dep` is never visited at all.
//
// On a state with no refs anywhere nothing matches, unwrap is the identity and
// the output is exactly what JSON.stringify would have produced on its own.
const stateReaderJS = `(path) => {
	const isRef = (o) => o !== null && typeof o === 'object' &&
		(o.__v_isRef === true || ('_rawValue' in o && '_value' in o));
	const unwrap = (o) => {
		for (let i = 0; i < 4 && isRef(o); i++) {
			o = o.value !== undefined ? o.value
				: (o._rawValue !== undefined ? o._rawValue : o._value);
		}
		return o;
	};
	let cur = unwrap(window.__INITIAL_STATE__);
	for (const key of path.split('.')) {
		if (cur === null || cur === undefined) return "";
		cur = unwrap(cur[key]);
	}
	if (cur === null || cur === undefined) return "";
	return JSON.stringify(cur, (k, v) => (isRef(v) ? unwrap(v) : v));
}`

// readStateJSON reads the value at path inside __INITIAL_STATE__ and returns it
// as JSON text.
//
// path is dot-separated, e.g. "feed.feeds" or
// "notification.notificationMap.mentions". A missing path is not an error: it
// returns an empty string and the caller decides whether that counts as a
// failure. Only a failing eval (page gone, context cancelled) returns an error.
func readStateJSON(page *rod.Page, path string) (string, error) {
	res, err := page.Eval(stateReaderJS, path)
	if err != nil {
		return "", fmt.Errorf("read __INITIAL_STATE__ %q failed: %w", path, err)
	}
	return res.Value.Str(), nil
}

// readState reads the value at path and unmarshals it into out.
//
// A false return means the path does not exist -- out is left untouched -- and
// is not an error. A failed unmarshal is an error.
func readState[T any](page *rod.Page, path string, out *T) (bool, error) {
	raw, err := readStateJSON(page, path)
	if err != nil {
		return false, err
	}
	if raw == "" {
		return false, nil
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return false, fmt.Errorf("unmarshal __INITIAL_STATE__ %q failed: %w", path, err)
	}
	return true, nil
}

// stateWaitInterval is waitState's polling interval.
const stateWaitInterval = 200 * time.Millisecond

// waitState waits for the value at path to appear, until it does, the timeout
// expires, or ctx is cancelled.
//
// It replaces the former MustWait(`__INITIAL_STATE__ !== undefined`): that
// condition is true from the first paint, so it returned immediately and waited
// for nothing. Waiting on a concrete path is what actually waits for hydration.
func waitState(ctx context.Context, page *rod.Page, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		raw, err := readStateJSON(page, path)
		if err == nil && raw != "" {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("wait for __INITIAL_STATE__ %q timed out: %w", path, err)
			}
			return fmt.Errorf("wait for __INITIAL_STATE__ %q timed out after %s", path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(stateWaitInterval):
		}
	}
}
