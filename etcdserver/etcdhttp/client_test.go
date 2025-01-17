/*
   Copyright 2014 CoreOS, Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package etcdhttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coreos/etcd/Godeps/_workspace/src/code.google.com/p/go.net/context"
	"github.com/coreos/etcd/Godeps/_workspace/src/github.com/jonboulle/clockwork"
	etcdErr "github.com/coreos/etcd/error"
	"github.com/coreos/etcd/etcdserver"
	"github.com/coreos/etcd/etcdserver/etcdhttp/httptypes"
	"github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/store"
	"github.com/coreos/etcd/version"
)

func mustMarshalEvent(t *testing.T, ev *store.Event) string {
	b := new(bytes.Buffer)
	if err := json.NewEncoder(b).Encode(ev); err != nil {
		t.Fatalf("error marshalling event %#v: %v", ev, err)
	}
	return b.String()
}

// mustNewForm takes a set of Values and constructs a PUT *http.Request,
// with a URL constructed from appending the given path to the standard keysPrefix
func mustNewForm(t *testing.T, p string, vals url.Values) *http.Request {
	u := mustNewURL(t, path.Join(keysPrefix, p))
	req, err := http.NewRequest("PUT", u.String(), strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err != nil {
		t.Fatalf("error creating new request: %v", err)
	}
	return req
}

// mustNewRequest takes a path, appends it to the standard keysPrefix, and constructs
// a GET *http.Request referencing the resulting URL
func mustNewRequest(t *testing.T, p string) *http.Request {
	return mustNewMethodRequest(t, "GET", p)
}

func mustNewMethodRequest(t *testing.T, m, p string) *http.Request {
	return &http.Request{
		Method: m,
		URL:    mustNewURL(t, path.Join(keysPrefix, p)),
	}
}

type serverRecorder struct {
	actions []action
}

func (s *serverRecorder) Do(_ context.Context, r etcdserverpb.Request) (etcdserver.Response, error) {
	s.actions = append(s.actions, action{name: "Do", params: []interface{}{r}})
	return etcdserver.Response{}, nil
}
func (s *serverRecorder) Process(_ context.Context, m raftpb.Message) error {
	s.actions = append(s.actions, action{name: "Process", params: []interface{}{m}})
	return nil
}
func (s *serverRecorder) Start() {}
func (s *serverRecorder) Stop()  {}
func (s *serverRecorder) AddMember(_ context.Context, m etcdserver.Member) error {
	s.actions = append(s.actions, action{name: "AddMember", params: []interface{}{m}})
	return nil
}
func (s *serverRecorder) RemoveMember(_ context.Context, id uint64) error {
	s.actions = append(s.actions, action{name: "RemoveMember", params: []interface{}{id}})
	return nil
}

type action struct {
	name   string
	params []interface{}
}

// flushingRecorder provides a channel to allow users to block until the Recorder is Flushed()
type flushingRecorder struct {
	*httptest.ResponseRecorder
	ch chan struct{}
}

func (fr *flushingRecorder) Flush() {
	fr.ResponseRecorder.Flush()
	fr.ch <- struct{}{}
}

// resServer implements the etcd.Server interface for testing.
// It returns the given responsefrom any Do calls, and nil error
type resServer struct {
	res etcdserver.Response
}

func (rs *resServer) Do(_ context.Context, _ etcdserverpb.Request) (etcdserver.Response, error) {
	return rs.res, nil
}
func (rs *resServer) Process(_ context.Context, _ raftpb.Message) error      { return nil }
func (rs *resServer) Start()                                                 {}
func (rs *resServer) Stop()                                                  {}
func (rs *resServer) AddMember(_ context.Context, _ etcdserver.Member) error { return nil }
func (rs *resServer) RemoveMember(_ context.Context, _ uint64) error         { return nil }

func boolp(b bool) *bool { return &b }

type dummyRaftTimer struct{}

func (drt dummyRaftTimer) Index() uint64 { return uint64(100) }
func (drt dummyRaftTimer) Term() uint64  { return uint64(5) }

type dummyWatcher struct {
	echan chan *store.Event
	sidx  uint64
}

func (w *dummyWatcher) EventChan() chan *store.Event {
	return w.echan
}
func (w *dummyWatcher) StartIndex() uint64 { return w.sidx }
func (w *dummyWatcher) Remove()            {}

func TestBadParseRequest(t *testing.T) {
	tests := []struct {
		in    *http.Request
		wcode int
	}{
		{
			// parseForm failure
			&http.Request{
				Body:   nil,
				Method: "PUT",
			},
			etcdErr.EcodeInvalidForm,
		},
		{
			// bad key prefix
			&http.Request{
				URL: mustNewURL(t, "/badprefix/"),
			},
			etcdErr.EcodeInvalidForm,
		},
		// bad values for prevIndex, waitIndex, ttl
		{
			mustNewForm(t, "foo", url.Values{"prevIndex": []string{"garbage"}}),
			etcdErr.EcodeIndexNaN,
		},
		{
			mustNewForm(t, "foo", url.Values{"prevIndex": []string{"1.5"}}),
			etcdErr.EcodeIndexNaN,
		},
		{
			mustNewForm(t, "foo", url.Values{"prevIndex": []string{"-1"}}),
			etcdErr.EcodeIndexNaN,
		},
		{
			mustNewForm(t, "foo", url.Values{"waitIndex": []string{"garbage"}}),
			etcdErr.EcodeIndexNaN,
		},
		{
			mustNewForm(t, "foo", url.Values{"waitIndex": []string{"??"}}),
			etcdErr.EcodeIndexNaN,
		},
		{
			mustNewForm(t, "foo", url.Values{"ttl": []string{"-1"}}),
			etcdErr.EcodeTTLNaN,
		},
		// bad values for recursive, sorted, wait, prevExist, dir, stream
		{
			mustNewForm(t, "foo", url.Values{"recursive": []string{"hahaha"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"recursive": []string{"1234"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"recursive": []string{"?"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"sorted": []string{"?"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"sorted": []string{"x"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"wait": []string{"?!"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"wait": []string{"yes"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"prevExist": []string{"yes"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"prevExist": []string{"#2"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"dir": []string{"no"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"dir": []string{"file"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"quorum": []string{"no"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"quorum": []string{"file"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"stream": []string{"zzz"}}),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewForm(t, "foo", url.Values{"stream": []string{"something"}}),
			etcdErr.EcodeInvalidField,
		},
		// prevValue cannot be empty
		{
			mustNewForm(t, "foo", url.Values{"prevValue": []string{""}}),
			etcdErr.EcodeInvalidField,
		},
		// wait is only valid with GET requests
		{
			mustNewMethodRequest(t, "HEAD", "foo?wait=true"),
			etcdErr.EcodeInvalidField,
		},
		// query values are considered
		{
			mustNewRequest(t, "foo?prevExist=wrong"),
			etcdErr.EcodeInvalidField,
		},
		{
			mustNewRequest(t, "foo?ttl=wrong"),
			etcdErr.EcodeTTLNaN,
		},
		// but body takes precedence if both are specified
		{
			mustNewForm(
				t,
				"foo?ttl=12",
				url.Values{"ttl": []string{"garbage"}},
			),
			etcdErr.EcodeTTLNaN,
		},
		{
			mustNewForm(
				t,
				"foo?prevExist=false",
				url.Values{"prevExist": []string{"yes"}},
			),
			etcdErr.EcodeInvalidField,
		},
	}
	for i, tt := range tests {
		got, err := parseKeyRequest(tt.in, 1234, clockwork.NewFakeClock())
		if err == nil {
			t.Errorf("#%d: unexpected nil error!", i)
			continue
		}
		ee, ok := err.(*etcdErr.Error)
		if !ok {
			t.Errorf("#%d: err is not etcd.Error!", i)
			continue
		}
		if ee.ErrorCode != tt.wcode {
			t.Errorf("#%d: code=%d, want %v", i, ee.ErrorCode, tt.wcode)
			t.Logf("cause: %#v", ee.Cause)
		}
		if !reflect.DeepEqual(got, etcdserverpb.Request{}) {
			t.Errorf("#%d: unexpected non-empty Request: %#v", i, got)
		}
	}
}

func TestGoodParseRequest(t *testing.T) {
	fc := clockwork.NewFakeClock()
	fc.Advance(1111)
	tests := []struct {
		in *http.Request
		w  etcdserverpb.Request
	}{
		{
			// good prefix, all other values default
			mustNewRequest(t, "foo"),
			etcdserverpb.Request{
				ID:     1234,
				Method: "GET",
				Path:   path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// value specified
			mustNewForm(
				t,
				"foo",
				url.Values{"value": []string{"some_value"}},
			),
			etcdserverpb.Request{
				ID:     1234,
				Method: "PUT",
				Val:    "some_value",
				Path:   path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// prevIndex specified
			mustNewForm(
				t,
				"foo",
				url.Values{"prevIndex": []string{"98765"}},
			),
			etcdserverpb.Request{
				ID:        1234,
				Method:    "PUT",
				PrevIndex: 98765,
				Path:      path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// recursive specified
			mustNewForm(
				t,
				"foo",
				url.Values{"recursive": []string{"true"}},
			),
			etcdserverpb.Request{
				ID:        1234,
				Method:    "PUT",
				Recursive: true,
				Path:      path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// sorted specified
			mustNewForm(
				t,
				"foo",
				url.Values{"sorted": []string{"true"}},
			),
			etcdserverpb.Request{
				ID:     1234,
				Method: "PUT",
				Sorted: true,
				Path:   path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// quorum specified
			mustNewForm(
				t,
				"foo",
				url.Values{"quorum": []string{"true"}},
			),
			etcdserverpb.Request{
				ID:     1234,
				Method: "PUT",
				Quorum: true,
				Path:   path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// wait specified
			mustNewRequest(t, "foo?wait=true"),
			etcdserverpb.Request{
				ID:     1234,
				Method: "GET",
				Wait:   true,
				Path:   path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// empty TTL specified
			mustNewRequest(t, "foo?ttl="),
			etcdserverpb.Request{
				ID:         1234,
				Method:     "GET",
				Path:       path.Join(etcdserver.StoreKeysPrefix, "/foo"),
				Expiration: 0,
			},
		},
		{
			// non-empty TTL specified
			mustNewRequest(t, "foo?ttl=5678"),
			etcdserverpb.Request{
				ID:         1234,
				Method:     "GET",
				Path:       path.Join(etcdserver.StoreKeysPrefix, "/foo"),
				Expiration: fc.Now().Add(5678 * time.Second).UnixNano(),
			},
		},
		{
			// zero TTL specified
			mustNewRequest(t, "foo?ttl=0"),
			etcdserverpb.Request{
				ID:         1234,
				Method:     "GET",
				Path:       path.Join(etcdserver.StoreKeysPrefix, "/foo"),
				Expiration: fc.Now().UnixNano(),
			},
		},
		{
			// dir specified
			mustNewRequest(t, "foo?dir=true"),
			etcdserverpb.Request{
				ID:     1234,
				Method: "GET",
				Dir:    true,
				Path:   path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// dir specified negatively
			mustNewRequest(t, "foo?dir=false"),
			etcdserverpb.Request{
				ID:     1234,
				Method: "GET",
				Dir:    false,
				Path:   path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// prevExist should be non-null if specified
			mustNewForm(
				t,
				"foo",
				url.Values{"prevExist": []string{"true"}},
			),
			etcdserverpb.Request{
				ID:        1234,
				Method:    "PUT",
				PrevExist: boolp(true),
				Path:      path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		{
			// prevExist should be non-null if specified
			mustNewForm(
				t,
				"foo",
				url.Values{"prevExist": []string{"false"}},
			),
			etcdserverpb.Request{
				ID:        1234,
				Method:    "PUT",
				PrevExist: boolp(false),
				Path:      path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		// mix various fields
		{
			mustNewForm(
				t,
				"foo",
				url.Values{
					"value":     []string{"some value"},
					"prevExist": []string{"true"},
					"prevValue": []string{"previous value"},
				},
			),
			etcdserverpb.Request{
				ID:        1234,
				Method:    "PUT",
				PrevExist: boolp(true),
				PrevValue: "previous value",
				Val:       "some value",
				Path:      path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		// query parameters should be used if given
		{
			mustNewForm(
				t,
				"foo?prevValue=woof",
				url.Values{},
			),
			etcdserverpb.Request{
				ID:        1234,
				Method:    "PUT",
				PrevValue: "woof",
				Path:      path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
		// but form values should take precedence over query parameters
		{
			mustNewForm(
				t,
				"foo?prevValue=woof",
				url.Values{
					"prevValue": []string{"miaow"},
				},
			),
			etcdserverpb.Request{
				ID:        1234,
				Method:    "PUT",
				PrevValue: "miaow",
				Path:      path.Join(etcdserver.StoreKeysPrefix, "/foo"),
			},
		},
	}

	for i, tt := range tests {
		got, err := parseKeyRequest(tt.in, 1234, fc)
		if err != nil {
			t.Errorf("#%d: err = %v, want %v", i, err, nil)
		}
		if !reflect.DeepEqual(got, tt.w) {
			t.Errorf("#%d: request=%#v, want %#v", i, got, tt.w)
		}
	}
}

func TestServeAdminMembers(t *testing.T) {
	memb1 := etcdserver.Member{ID: 12, Attributes: etcdserver.Attributes{ClientURLs: []string{"http://localhost:8080"}}}
	memb2 := etcdserver.Member{ID: 13, Attributes: etcdserver.Attributes{ClientURLs: []string{"http://localhost:8081"}}}
	cluster := &fakeCluster{
		id:      1,
		members: map[uint64]*etcdserver.Member{1: &memb1, 2: &memb2},
	}
	h := &adminMembersHandler{
		server:      &serverRecorder{},
		clock:       clockwork.NewFakeClock(),
		clusterInfo: cluster,
	}

	wmc := string(`[{"id":"c","name":"","peerURLs":[],"clientURLs":["http://localhost:8080"]},{"id":"d","name":"","peerURLs":[],"clientURLs":["http://localhost:8081"]}]`)

	tests := []struct {
		path  string
		wcode int
		wct   string
		wbody string
	}{
		{adminMembersPrefix, http.StatusOK, "application/json", wmc + "\n"},
		{adminMembersPrefix + "/", http.StatusOK, "application/json", wmc + "\n"},
		{path.Join(adminMembersPrefix, "100"), http.StatusNotFound, "application/json", `{"message":"Not found"}`},
		{path.Join(adminMembersPrefix, "foobar"), http.StatusNotFound, "application/json", `{"message":"Not found"}`},
	}

	for i, tt := range tests {
		req, err := http.NewRequest("GET", mustNewURL(t, tt.path).String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)

		if rw.Code != tt.wcode {
			t.Errorf("#%d: code=%d, want %d", i, rw.Code, tt.wcode)
		}
		if gct := rw.Header().Get("Content-Type"); gct != tt.wct {
			t.Errorf("#%d: content-type = %s, want %s", i, gct, tt.wct)
		}
		gcid := rw.Header().Get("X-Etcd-Cluster-ID")
		wcid := strconv.FormatUint(cluster.ID(), 16)
		if gcid != wcid {
			t.Errorf("#%d: cid = %s, want %s", i, gcid, wcid)
		}
		if rw.Body.String() != tt.wbody {
			t.Errorf("#%d: body = %q, want %q", i, rw.Body.String(), tt.wbody)
		}
	}
}

func TestServeAdminMembersPut(t *testing.T) {
	u := mustNewURL(t, adminMembersPrefix)
	raftAttr := etcdserver.RaftAttributes{PeerURLs: []string{"http://127.0.0.1:1"}}
	b, err := json.Marshal(raftAttr)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewReader(b)
	req, err := http.NewRequest("POST", u.String(), body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	s := &serverRecorder{}
	h := &adminMembersHandler{
		server:      s,
		clock:       clockwork.NewFakeClock(),
		clusterInfo: &fakeCluster{id: 1},
	}
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	wcode := http.StatusCreated
	if rw.Code != wcode {
		t.Errorf("code=%d, want %d", rw.Code, wcode)
	}
	wm := etcdserver.Member{
		ID:             3064321551348478165,
		RaftAttributes: raftAttr,
	}

	wb, err := json.Marshal(wm)
	if err != nil {
		t.Fatal(err)
	}
	wct := "application/json"
	if gct := rw.Header().Get("Content-Type"); gct != wct {
		t.Errorf("content-type = %s, want %s", gct, wct)
	}
	gcid := rw.Header().Get("X-Etcd-Cluster-ID")
	wcid := strconv.FormatUint(h.clusterInfo.ID(), 16)
	if gcid != wcid {
		t.Errorf("cid = %s, want %s", gcid, wcid)
	}
	g := rw.Body.String()
	w := string(wb) + "\n"
	if g != w {
		t.Errorf("got body=%q, want %q", g, w)
	}
	wactions := []action{{name: "AddMember", params: []interface{}{wm}}}
	if !reflect.DeepEqual(s.actions, wactions) {
		t.Errorf("actions = %+v, want %+v", s.actions, wactions)
	}
}

func TestServeAdminMembersDelete(t *testing.T) {
	req := &http.Request{
		Method: "DELETE",
		URL:    mustNewURL(t, path.Join(adminMembersPrefix, "BEEF")),
	}
	s := &serverRecorder{}
	h := &adminMembersHandler{
		server:      s,
		clusterInfo: &fakeCluster{id: 1},
	}
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	wcode := http.StatusNoContent
	if rw.Code != wcode {
		t.Errorf("code=%d, want %d", rw.Code, wcode)
	}
	gcid := rw.Header().Get("X-Etcd-Cluster-ID")
	wcid := strconv.FormatUint(h.clusterInfo.ID(), 16)
	if gcid != wcid {
		t.Errorf("cid = %s, want %s", gcid, wcid)
	}
	g := rw.Body.String()
	if g != "" {
		t.Errorf("got body=%q, want %q", g, "")
	}
	wactions := []action{{name: "RemoveMember", params: []interface{}{uint64(0xBEEF)}}}
	if !reflect.DeepEqual(s.actions, wactions) {
		t.Errorf("actions = %+v, want %+v", s.actions, wactions)
	}
}

func TestServeAdminMembersFail(t *testing.T) {
	tests := []struct {
		req    *http.Request
		server etcdserver.Server

		wcode int
	}{
		{
			// bad method
			&http.Request{
				Method: "CONNECT",
			},
			&resServer{},

			http.StatusMethodNotAllowed,
		},
		{
			// bad method
			&http.Request{
				Method: "TRACE",
			},
			&resServer{},

			http.StatusMethodNotAllowed,
		},
		{
			// parse body error
			&http.Request{
				URL:    mustNewURL(t, adminMembersPrefix),
				Method: "POST",
				Body:   ioutil.NopCloser(strings.NewReader("bad json")),
			},
			&resServer{},

			http.StatusBadRequest,
		},
		{
			// bad content type
			&http.Request{
				URL:    mustNewURL(t, adminMembersPrefix),
				Method: "POST",
				Body:   ioutil.NopCloser(strings.NewReader(`{"PeerURLs": ["http://127.0.0.1:1"]}`)),
				Header: map[string][]string{"Content-Type": []string{"application/bad"}},
			},
			&errServer{},

			http.StatusBadRequest,
		},
		{
			// bad url
			&http.Request{
				URL:    mustNewURL(t, adminMembersPrefix),
				Method: "POST",
				Body:   ioutil.NopCloser(strings.NewReader(`{"PeerURLs": ["http://a"]}`)),
				Header: map[string][]string{"Content-Type": []string{"application/json"}},
			},
			&errServer{},

			http.StatusBadRequest,
		},
		{
			// etcdserver.AddMember error
			&http.Request{
				URL:    mustNewURL(t, adminMembersPrefix),
				Method: "POST",
				Body:   ioutil.NopCloser(strings.NewReader(`{"PeerURLs": ["http://127.0.0.1:1"]}`)),
				Header: map[string][]string{"Content-Type": []string{"application/json"}},
			},
			&errServer{
				errors.New("blah"),
			},

			http.StatusInternalServerError,
		},
		{
			// etcdserver.RemoveMember error
			&http.Request{
				URL:    mustNewURL(t, path.Join(adminMembersPrefix, "1")),
				Method: "DELETE",
			},
			&errServer{
				errors.New("blah"),
			},

			http.StatusInternalServerError,
		},
		{
			// etcdserver.RemoveMember error
			&http.Request{
				URL:    mustNewURL(t, adminMembersPrefix),
				Method: "DELETE",
			},
			nil,

			http.StatusMethodNotAllowed,
		},
	}
	for i, tt := range tests {
		h := &adminMembersHandler{
			server:      tt.server,
			clusterInfo: &fakeCluster{id: 1},
			clock:       clockwork.NewFakeClock(),
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, tt.req)
		if rw.Code != tt.wcode {
			t.Errorf("#%d: code=%d, want %d", i, rw.Code, tt.wcode)
		}
		if rw.Code != http.StatusMethodNotAllowed {
			gcid := rw.Header().Get("X-Etcd-Cluster-ID")
			wcid := strconv.FormatUint(h.clusterInfo.ID(), 16)
			if gcid != wcid {
				t.Errorf("#%d: cid = %s, want %s", i, gcid, wcid)
			}
		}
	}
}

func TestWriteEvent(t *testing.T) {
	// nil event should not panic
	rw := httptest.NewRecorder()
	writeKeyEvent(rw, nil, dummyRaftTimer{})
	h := rw.Header()
	if len(h) > 0 {
		t.Fatalf("unexpected non-empty headers: %#v", h)
	}
	b := rw.Body.String()
	if len(b) > 0 {
		t.Fatalf("unexpected non-empty body: %q", b)
	}

	tests := []struct {
		ev  *store.Event
		idx string
		// TODO(jonboulle): check body as well as just status code
		code int
		err  error
	}{
		// standard case, standard 200 response
		{
			&store.Event{
				Action:   store.Get,
				Node:     &store.NodeExtern{},
				PrevNode: &store.NodeExtern{},
			},
			"0",
			http.StatusOK,
			nil,
		},
		// check new nodes return StatusCreated
		{
			&store.Event{
				Action:   store.Create,
				Node:     &store.NodeExtern{},
				PrevNode: &store.NodeExtern{},
			},
			"0",
			http.StatusCreated,
			nil,
		},
	}

	for i, tt := range tests {
		rw := httptest.NewRecorder()
		writeKeyEvent(rw, tt.ev, dummyRaftTimer{})
		if gct := rw.Header().Get("Content-Type"); gct != "application/json" {
			t.Errorf("case %d: bad Content-Type: got %q, want application/json", i, gct)
		}
		if gri := rw.Header().Get("X-Raft-Index"); gri != "100" {
			t.Errorf("case %d: bad X-Raft-Index header: got %s, want %s", i, gri, "100")
		}
		if grt := rw.Header().Get("X-Raft-Term"); grt != "5" {
			t.Errorf("case %d: bad X-Raft-Term header: got %s, want %s", i, grt, "5")
		}
		if gei := rw.Header().Get("X-Etcd-Index"); gei != tt.idx {
			t.Errorf("case %d: bad X-Etcd-Index header: got %s, want %s", i, gei, tt.idx)
		}
		if rw.Code != tt.code {
			t.Errorf("case %d: bad response code: got %d, want %v", i, rw.Code, tt.code)
		}

	}
}

func TestV2DeprecatedMachinesEndpoint(t *testing.T) {
	tests := []struct {
		method string
		wcode  int
	}{
		{"GET", http.StatusOK},
		{"HEAD", http.StatusOK},
		{"POST", http.StatusMethodNotAllowed},
	}

	m := NewClientHandler(&etcdserver.EtcdServer{Cluster: &etcdserver.Cluster{}})
	s := httptest.NewServer(m)
	defer s.Close()

	for _, tt := range tests {
		req, err := http.NewRequest(tt.method, s.URL+deprecatedMachinesPrefix, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		if resp.StatusCode != tt.wcode {
			t.Errorf("StatusCode = %d, expected %d", resp.StatusCode, tt.wcode)
		}
	}
}

func TestServeMachines(t *testing.T) {
	cluster := &fakeCluster{
		clientURLs: []string{"http://localhost:8080", "http://localhost:8081", "http://localhost:8082"},
	}
	writer := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &deprecatedMachinesHandler{clusterInfo: cluster}
	h.ServeHTTP(writer, req)
	w := "http://localhost:8080, http://localhost:8081, http://localhost:8082"
	if g := writer.Body.String(); g != w {
		t.Errorf("body = %s, want %s", g, w)
	}
	if writer.Code != http.StatusOK {
		t.Errorf("code = %d, want %d", writer.Code, http.StatusOK)
	}
}

type dummyStats struct {
	data []byte
}

func (ds *dummyStats) SelfStats() []byte               { return ds.data }
func (ds *dummyStats) LeaderStats() []byte             { return ds.data }
func (ds *dummyStats) StoreStats() []byte              { return ds.data }
func (ds *dummyStats) UpdateRecvApp(_ uint64, _ int64) {}

func TestServeSelfStats(t *testing.T) {
	wb := []byte("some statistics")
	w := string(wb)
	sh := &statsHandler{
		stats: &dummyStats{data: wb},
	}
	rw := httptest.NewRecorder()
	sh.serveSelf(rw, &http.Request{Method: "GET"})
	if rw.Code != http.StatusOK {
		t.Errorf("code = %d, want %d", rw.Code, http.StatusOK)
	}
	wct := "application/json"
	if gct := rw.Header().Get("Content-Type"); gct != wct {
		t.Errorf("Content-Type = %q, want %q", gct, wct)
	}
	if g := rw.Body.String(); g != w {
		t.Errorf("body = %s, want %s", g, w)
	}
}

func TestSelfServeStatsBad(t *testing.T) {
	for _, m := range []string{"PUT", "POST", "DELETE"} {
		sh := &statsHandler{}
		rw := httptest.NewRecorder()
		sh.serveSelf(
			rw,
			&http.Request{
				Method: m,
			},
		)
		if rw.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: code=%d, want %d", m, rw.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestLeaderServeStatsBad(t *testing.T) {
	for _, m := range []string{"PUT", "POST", "DELETE"} {
		sh := &statsHandler{}
		rw := httptest.NewRecorder()
		sh.serveLeader(
			rw,
			&http.Request{
				Method: m,
			},
		)
		if rw.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: code=%d, want %d", m, rw.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestServeLeaderStats(t *testing.T) {
	wb := []byte("some statistics")
	w := string(wb)
	sh := &statsHandler{
		stats: &dummyStats{data: wb},
	}
	rw := httptest.NewRecorder()
	sh.serveLeader(rw, &http.Request{Method: "GET"})
	if rw.Code != http.StatusOK {
		t.Errorf("code = %d, want %d", rw.Code, http.StatusOK)
	}
	wct := "application/json"
	if gct := rw.Header().Get("Content-Type"); gct != wct {
		t.Errorf("Content-Type = %q, want %q", gct, wct)
	}
	if g := rw.Body.String(); g != w {
		t.Errorf("body = %s, want %s", g, w)
	}
}

func TestServeStoreStats(t *testing.T) {
	wb := []byte("some statistics")
	w := string(wb)
	sh := &statsHandler{
		stats: &dummyStats{data: wb},
	}
	rw := httptest.NewRecorder()
	sh.serveStore(rw, &http.Request{Method: "GET"})
	if rw.Code != http.StatusOK {
		t.Errorf("code = %d, want %d", rw.Code, http.StatusOK)
	}
	wct := "application/json"
	if gct := rw.Header().Get("Content-Type"); gct != wct {
		t.Errorf("Content-Type = %q, want %q", gct, wct)
	}
	if g := rw.Body.String(); g != w {
		t.Errorf("body = %s, want %s", g, w)
	}

}

func TestServeVersion(t *testing.T) {
	req, err := http.NewRequest("GET", "", nil)
	if err != nil {
		t.Fatalf("error creating request: %v", err)
	}
	rw := httptest.NewRecorder()
	serveVersion(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("code=%d, want %d", rw.Code, http.StatusOK)
	}
	w := fmt.Sprintf("etcd %s", version.Version)
	if g := rw.Body.String(); g != w {
		t.Fatalf("body = %q, want %q", g, w)
	}
}

func TestServeVersionFails(t *testing.T) {
	for _, m := range []string{
		"CONNECT", "TRACE", "PUT", "POST", "HEAD",
	} {
		req, err := http.NewRequest(m, "", nil)
		if err != nil {
			t.Fatalf("error creating request: %v", err)
		}
		rw := httptest.NewRecorder()
		serveVersion(rw, req)
		if rw.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: code=%d, want %d", m, rw.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestBadServeKeys(t *testing.T) {
	testBadCases := []struct {
		req    *http.Request
		server etcdserver.Server

		wcode int
		wbody string
	}{
		{
			// bad method
			&http.Request{
				Method: "CONNECT",
			},
			&resServer{},

			http.StatusMethodNotAllowed,
			"Method Not Allowed",
		},
		{
			// bad method
			&http.Request{
				Method: "TRACE",
			},
			&resServer{},

			http.StatusMethodNotAllowed,
			"Method Not Allowed",
		},
		{
			// parseRequest error
			&http.Request{
				Body:   nil,
				Method: "PUT",
			},
			&resServer{},

			http.StatusBadRequest,
			`{"errorCode":210,"message":"Invalid POST form","cause":"missing form body","index":0}`,
		},
		{
			// etcdserver.Server error
			mustNewRequest(t, "foo"),
			&errServer{
				errors.New("blah"),
			},

			http.StatusInternalServerError,
			"Internal Server Error",
		},
		{
			// etcdserver.Server etcd error
			mustNewRequest(t, "foo"),
			&errServer{
				etcdErr.NewError(etcdErr.EcodeKeyNotFound, "/1/pant", 0),
			},

			http.StatusNotFound,
			`{"errorCode":100,"message":"Key not found","cause":"/pant","index":0}`,
		},
		{
			// non-event/watcher response from etcdserver.Server
			mustNewRequest(t, "foo"),
			&resServer{
				etcdserver.Response{},
			},

			http.StatusInternalServerError,
			"Internal Server Error",
		},
	}
	for i, tt := range testBadCases {
		h := &keysHandler{
			timeout:     0, // context times out immediately
			server:      tt.server,
			clusterInfo: &fakeCluster{id: 1},
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, tt.req)
		if rw.Code != tt.wcode {
			t.Errorf("#%d: got code=%d, want %d", i, rw.Code, tt.wcode)
		}
		if rw.Code != http.StatusMethodNotAllowed {
			gcid := rw.Header().Get("X-Etcd-Cluster-ID")
			wcid := strconv.FormatUint(h.clusterInfo.ID(), 16)
			if gcid != wcid {
				t.Errorf("#%d: cid = %s, want %s", i, gcid, wcid)
			}
		}
		if g := strings.TrimSuffix(rw.Body.String(), "\n"); g != tt.wbody {
			t.Errorf("#%d: body = %s, want %s", i, g, tt.wbody)
		}
	}
}

func TestServeKeysEvent(t *testing.T) {
	req := mustNewRequest(t, "foo")
	server := &resServer{
		etcdserver.Response{
			Event: &store.Event{
				Action: store.Get,
				Node:   &store.NodeExtern{},
			},
		},
	}
	h := &keysHandler{
		timeout:     time.Hour,
		server:      server,
		clusterInfo: &fakeCluster{id: 1},
		timer:       &dummyRaftTimer{},
	}
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	wcode := http.StatusOK
	wbody := mustMarshalEvent(
		t,
		&store.Event{
			Action: store.Get,
			Node:   &store.NodeExtern{},
		},
	)

	if rw.Code != wcode {
		t.Errorf("got code=%d, want %d", rw.Code, wcode)
	}
	gcid := rw.Header().Get("X-Etcd-Cluster-ID")
	wcid := strconv.FormatUint(h.clusterInfo.ID(), 16)
	if gcid != wcid {
		t.Errorf("cid = %s, want %s", gcid, wcid)
	}
	g := rw.Body.String()
	if g != wbody {
		t.Errorf("got body=%#v, want %#v", g, wbody)
	}
}

func TestServeKeysWatch(t *testing.T) {
	req := mustNewRequest(t, "/foo/bar")
	ec := make(chan *store.Event)
	dw := &dummyWatcher{
		echan: ec,
	}
	server := &resServer{
		etcdserver.Response{
			Watcher: dw,
		},
	}
	h := &keysHandler{
		timeout:     time.Hour,
		server:      server,
		clusterInfo: &fakeCluster{id: 1},
		timer:       &dummyRaftTimer{},
	}
	go func() {
		ec <- &store.Event{
			Action: store.Get,
			Node:   &store.NodeExtern{},
		}
	}()
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	wcode := http.StatusOK
	wbody := mustMarshalEvent(
		t,
		&store.Event{
			Action: store.Get,
			Node:   &store.NodeExtern{},
		},
	)

	if rw.Code != wcode {
		t.Errorf("got code=%d, want %d", rw.Code, wcode)
	}
	gcid := rw.Header().Get("X-Etcd-Cluster-ID")
	wcid := strconv.FormatUint(h.clusterInfo.ID(), 16)
	if gcid != wcid {
		t.Errorf("cid = %s, want %s", gcid, wcid)
	}
	g := rw.Body.String()
	if g != wbody {
		t.Errorf("got body=%#v, want %#v", g, wbody)
	}
}

type recordingCloseNotifier struct {
	*httptest.ResponseRecorder
	cn chan bool
}

func (rcn *recordingCloseNotifier) CloseNotify() <-chan bool {
	return rcn.cn
}

func TestHandleWatch(t *testing.T) {
	defaultRwRr := func() (http.ResponseWriter, *httptest.ResponseRecorder) {
		r := httptest.NewRecorder()
		return r, r
	}
	noopEv := func(chan *store.Event) {}

	tests := []struct {
		getCtx   func() context.Context
		getRwRr  func() (http.ResponseWriter, *httptest.ResponseRecorder)
		doToChan func(chan *store.Event)

		wbody string
	}{
		{
			// Normal case: one event
			context.Background,
			defaultRwRr,
			func(ch chan *store.Event) {
				ch <- &store.Event{
					Action: store.Get,
					Node:   &store.NodeExtern{},
				}
			},

			mustMarshalEvent(
				t,
				&store.Event{
					Action: store.Get,
					Node:   &store.NodeExtern{},
				},
			),
		},
		{
			// Channel is closed, no event
			context.Background,
			defaultRwRr,
			func(ch chan *store.Event) {
				close(ch)
			},

			"",
		},
		{
			// Simulate a timed-out context
			func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			defaultRwRr,
			noopEv,

			"",
		},
		{
			// Close-notifying request
			context.Background,
			func() (http.ResponseWriter, *httptest.ResponseRecorder) {
				rw := &recordingCloseNotifier{
					ResponseRecorder: httptest.NewRecorder(),
					cn:               make(chan bool, 1),
				}
				rw.cn <- true
				return rw, rw.ResponseRecorder
			},
			noopEv,

			"",
		},
	}

	for i, tt := range tests {
		rw, rr := tt.getRwRr()
		wa := &dummyWatcher{
			echan: make(chan *store.Event, 1),
			sidx:  10,
		}
		tt.doToChan(wa.echan)

		handleKeyWatch(tt.getCtx(), rw, wa, false, dummyRaftTimer{})

		wcode := http.StatusOK
		wct := "application/json"
		wei := "10"
		wri := "100"
		wrt := "5"

		if rr.Code != wcode {
			t.Errorf("#%d: got code=%d, want %d", i, rr.Code, wcode)
		}
		h := rr.Header()
		if ct := h.Get("Content-Type"); ct != wct {
			t.Errorf("#%d: Content-Type=%q, want %q", i, ct, wct)
		}
		if ei := h.Get("X-Etcd-Index"); ei != wei {
			t.Errorf("#%d: X-Etcd-Index=%q, want %q", i, ei, wei)
		}
		if ri := h.Get("X-Raft-Index"); ri != wri {
			t.Errorf("#%d: X-Raft-Index=%q, want %q", i, ri, wri)
		}
		if rt := h.Get("X-Raft-Term"); rt != wrt {
			t.Errorf("#%d: X-Raft-Term=%q, want %q", i, rt, wrt)
		}
		g := rr.Body.String()
		if g != tt.wbody {
			t.Errorf("#%d: got body=%#v, want %#v", i, g, tt.wbody)
		}
	}
}

func TestHandleWatchStreaming(t *testing.T) {
	rw := &flushingRecorder{
		httptest.NewRecorder(),
		make(chan struct{}, 1),
	}
	wa := &dummyWatcher{
		echan: make(chan *store.Event),
	}

	// Launch the streaming handler in the background with a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		handleKeyWatch(ctx, rw, wa, true, dummyRaftTimer{})
		close(done)
	}()

	// Expect one Flush for the headers etc.
	select {
	case <-rw.ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for flush")
	}

	// Expect headers but no body
	wcode := http.StatusOK
	wct := "application/json"
	wbody := ""

	if rw.Code != wcode {
		t.Errorf("got code=%d, want %d", rw.Code, wcode)
	}
	h := rw.Header()
	if ct := h.Get("Content-Type"); ct != wct {
		t.Errorf("Content-Type=%q, want %q", ct, wct)
	}
	g := rw.Body.String()
	if g != wbody {
		t.Errorf("got body=%#v, want %#v", g, wbody)
	}

	// Now send the first event
	select {
	case wa.echan <- &store.Event{
		Action: store.Get,
		Node:   &store.NodeExtern{},
	}:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for send")
	}

	// Wait for it to be flushed...
	select {
	case <-rw.ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for flush")
	}

	// And check the body is as expected
	wbody = mustMarshalEvent(
		t,
		&store.Event{
			Action: store.Get,
			Node:   &store.NodeExtern{},
		},
	)
	g = rw.Body.String()
	if g != wbody {
		t.Errorf("got body=%#v, want %#v", g, wbody)
	}

	// Rinse and repeat
	select {
	case wa.echan <- &store.Event{
		Action: store.Get,
		Node:   &store.NodeExtern{},
	}:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for send")
	}

	select {
	case <-rw.ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for flush")
	}

	// This time, we expect to see both events
	wbody = wbody + wbody
	g = rw.Body.String()
	if g != wbody {
		t.Errorf("got body=%#v, want %#v", g, wbody)
	}

	// Finally, time out the connection and ensure the serving goroutine returns
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for done")
	}
}

func TestTrimEventPrefix(t *testing.T) {
	pre := "/abc"
	tests := []struct {
		ev  *store.Event
		wev *store.Event
	}{
		{
			nil,
			nil,
		},
		{
			&store.Event{},
			&store.Event{},
		},
		{
			&store.Event{Node: &store.NodeExtern{Key: "/abc/def"}},
			&store.Event{Node: &store.NodeExtern{Key: "/def"}},
		},
		{
			&store.Event{PrevNode: &store.NodeExtern{Key: "/abc/ghi"}},
			&store.Event{PrevNode: &store.NodeExtern{Key: "/ghi"}},
		},
		{
			&store.Event{
				Node:     &store.NodeExtern{Key: "/abc/def"},
				PrevNode: &store.NodeExtern{Key: "/abc/ghi"},
			},
			&store.Event{
				Node:     &store.NodeExtern{Key: "/def"},
				PrevNode: &store.NodeExtern{Key: "/ghi"},
			},
		},
	}
	for i, tt := range tests {
		ev := trimEventPrefix(tt.ev, pre)
		if !reflect.DeepEqual(ev, tt.wev) {
			t.Errorf("#%d: event = %+v, want %+v", i, ev, tt.wev)
		}
	}
}

func TestTrimNodeExternPrefix(t *testing.T) {
	pre := "/abc"
	tests := []struct {
		n  *store.NodeExtern
		wn *store.NodeExtern
	}{
		{
			nil,
			nil,
		},
		{
			&store.NodeExtern{Key: "/abc/def"},
			&store.NodeExtern{Key: "/def"},
		},
		{
			&store.NodeExtern{
				Key: "/abc/def",
				Nodes: []*store.NodeExtern{
					{Key: "/abc/def/1"},
					{Key: "/abc/def/2"},
				},
			},
			&store.NodeExtern{
				Key: "/def",
				Nodes: []*store.NodeExtern{
					{Key: "/def/1"},
					{Key: "/def/2"},
				},
			},
		},
	}
	for i, tt := range tests {
		n := trimNodeExternPrefix(tt.n, pre)
		if !reflect.DeepEqual(n, tt.wn) {
			t.Errorf("#%d: node = %+v, want %+v", i, n, tt.wn)
		}
	}
}

func TestTrimPrefix(t *testing.T) {
	tests := []struct {
		in     string
		prefix string
		w      string
	}{
		{"/v2/admin/members", "/v2/admin/members", ""},
		{"/v2/admin/members/", "/v2/admin/members", ""},
		{"/v2/admin/members/foo", "/v2/admin/members", "foo"},
	}
	for i, tt := range tests {
		if g := trimPrefix(tt.in, tt.prefix); g != tt.w {
			t.Errorf("#%d: trimPrefix = %q, want %q", i, g, tt.w)
		}
	}
}

func TestNewMemberCollection(t *testing.T) {
	fixture := []*etcdserver.Member{
		&etcdserver.Member{
			ID:             12,
			Attributes:     etcdserver.Attributes{ClientURLs: []string{"http://localhost:8080", "http://localhost:8081"}},
			RaftAttributes: etcdserver.RaftAttributes{PeerURLs: []string{"http://localhost:8082", "http://localhost:8083"}},
		},
		&etcdserver.Member{
			ID:             13,
			Attributes:     etcdserver.Attributes{ClientURLs: []string{"http://localhost:9090", "http://localhost:9091"}},
			RaftAttributes: etcdserver.RaftAttributes{PeerURLs: []string{"http://localhost:9092", "http://localhost:9093"}},
		},
	}
	got := newMemberCollection(fixture)

	want := httptypes.MemberCollection([]httptypes.Member{
		httptypes.Member{
			ID:         "c",
			ClientURLs: []string{"http://localhost:8080", "http://localhost:8081"},
			PeerURLs:   []string{"http://localhost:8082", "http://localhost:8083"},
		},
		httptypes.Member{
			ID:         "d",
			ClientURLs: []string{"http://localhost:9090", "http://localhost:9091"},
			PeerURLs:   []string{"http://localhost:9092", "http://localhost:9093"},
		},
	})

	if !reflect.DeepEqual(want, got) {
		t.Fatalf("newMemberCollection failure: want=%#v, got=%#v", want, got)
	}
}
