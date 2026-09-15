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

// readStateJSON 读 __INITIAL_STATE__ 里 path 指向的值，返回其 JSON 文本。
//
// path 用点号分隔，例如 "feed.feeds"、"notification.notificationMap.mentions"。
// 路径不存在不是错误，返回空串——调用方自己决定这算不算失败。只有 eval 本身
// 失败（页面没了、上下文取消）才返回 error。
func readStateJSON(page *rod.Page, path string) (string, error) {
	res, err := page.Eval(stateReaderJS, path)
	if err != nil {
		return "", fmt.Errorf("read __INITIAL_STATE__ %q failed: %w", path, err)
	}
	return res.Value.Str(), nil
}

// readState 读 path 指向的值并反序列化到 out。
//
// 返回 false 表示路径不存在（out 不会被改动），不是错误；反序列化失败才是错误。
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

// stateWaitInterval 是 waitState 的轮询间隔。
const stateWaitInterval = 200 * time.Millisecond

// waitState 等 path 指向的值出现，直到出现、超时或 ctx 取消。
//
// 取代原先的 MustWait(`__INITIAL_STATE__ !== undefined`)：那个条件从首屏起就为真，
// 立即返回，等于没等。等具体路径才是真的在等注水。
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
