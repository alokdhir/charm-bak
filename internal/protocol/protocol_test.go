package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Each method's params and result types. A nil params type means the
// method takes none.
var methods = map[string]struct{ params, result any }{
	MethodHello:          {&HelloParams{}, &HelloResult{}},
	MethodStatus:         {nil, &StatusResult{}},
	MethodSources:        {nil, &SourcesResult{}},
	MethodSnapshots:      {&SnapshotsParams{}, &SnapshotsResult{}},
	MethodLs:             {&LsParams{}, &LsResult{}},
	MethodDump:           {&DumpParams{}, &DumpResult{}},
	MethodDiff:           {&DiffParams{}, &DiffResult{}},
	MethodRestore:        {&RestoreParams{}, &JobResult{}},
	MethodBackupNow:      {&BackupNowParams{}, &JobResult{}},
	MethodCancel:         {&CancelParams{}, &Empty{}},
	MethodSourceAdd:      {&SourceAddParams{}, &SourceResult{}},
	MethodSourceSet:      {&SourceSetParams{}, &SourceResult{}},
	MethodSourceRewrite:  {&SourceRewriteParams{}, &SourceRewriteResult{}},
	MethodSourceRemove:   {&SourceRemoveParams{}, &Empty{}},
	MethodSnapshotSet:    {&SnapshotSetParams{}, &SnapshotResult{}},
	MethodSnapshotMeta:   {&SnapshotMetaParams{}, &SnapshotMetaResult{}},
	MethodChanges:        {&ChangesParams{}, &ChangesResult{}},
	MethodPause:          {nil, &Empty{}},
	MethodResume:         {nil, &Empty{}},
	MethodHistory:        {&HistoryParams{}, &HistoryResult{}},
	MethodRun:            {&RunParams{}, &RunResult{}},
	MethodNoticeShow:     {&Notice{}, &Empty{}},
	MethodNoticeTake:     {nil, &NoticeTakeResult{}},
	MethodKeyRotate:      {&KeyRotateParams{}, &JobResult{}},
	MethodAWSAdopt:       {&AWSAdoptParams{}, &AWSAdoptResult{}},
	MethodSubscribe:      {nil, &Empty{}},
	MethodRepository:     {nil, &RepositoryResult{}},
	MethodDefaultsSet:    {&DefaultsSetParams{}, &DefaultsResult{}},
	MethodHosts:          {nil, &HostsResult{}},
	MethodPairStart:      {&PairStartParams{}, &PairStartResult{}},
	MethodHostRepair:     {&HostRepairParams{}, &PairStartResult{}},
	MethodRepoSet:        {&RepositorySetParams{}, &RepositoryResult{}},
	MethodSelfUpdate:     {&SelfUpdateParams{}, &SelfUpdateResult{}},
	MethodHostSet:        {&HostSetParams{}, &HostResult{}},
	MethodRepoProbe:      {&RepositoryProbeParams{}, &RepositoryProbeResult{}},
	MethodRepoSetup:      {&RepositorySetupParams{}, &RepositorySetupResult{}},
	MethodHostRemove:     {&HostRemoveParams{}, &HostRemoveResult{}},
	MethodUninstall:      {&UninstallParams{}, &Empty{}},
	MethodAccessSettings: {nil, &Empty{}},
	MethodAWSStatus:      {nil, &AWSStatusResult{}},
	MethodAWSKeys:        {&AWSKeysParams{}, &AWSKeysResult{}},
}

var errorData = map[string]any{
	ErrBusy:                &BusyData{},
	ErrResticFailed:        &ResticFailedData{},
	ErrUnsupportedProtocol: &UnsupportedProtocolData{},
}

type fixture struct {
	Request *json.RawMessage `json:"request,omitempty"`
	Reply   *json.RawMessage `json:"reply,omitempty"`
	Event   *json.RawMessage `json:"event,omitempty"`
}

const fixtureDir = "../../testdata/protocol"

// strict decodes b into v, rejecting fields v doesn't have, then checks
// that v encodes back to the same JSON, so a field the type always
// writes must also appear in the example.
func strict(t *testing.T, what string, b []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Errorf("%s: re-encode: %v", what, err)
		return
	}
	var want, got any
	_ = json.Unmarshal(b, &want)
	_ = json.Unmarshal(out, &got)
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%s: round trip differs\n want %s\n  got %s", what, compact(b), out)
	}
}

func compact(b []byte) string {
	var buf bytes.Buffer
	_ = json.Compact(&buf, b)
	return buf.String()
}

func TestFixturesMatchTypes(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	if len(files) == 0 {
		t.Fatal("no fixtures found")
	}
	covered := map[string]bool{}
	events := map[string]bool{}
	for _, f := range files {
		name := filepath.Base(f)
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var fx fixture
		strict(t, name, b, &fx)

		if fx.Event != nil {
			var e Event
			if err := json.Unmarshal(*fx.Event, &e); err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if e.payload() == nil {
				t.Errorf("%s: unknown event %q", name, e.Name)
				continue
			}
			// The payload alone (without "event") must match its type exactly.
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(*fx.Event, &fields)
			delete(fields, "event")
			delete(fields, "host")
			rest, _ := json.Marshal(fields)
			strict(t, name+" payload", rest, newOf(e.payload()))
			events[e.Name] = true
			// Event's own encoding must reproduce the example.
			out, _ := json.Marshal(e)
			var want, got any
			_ = json.Unmarshal(*fx.Event, &want)
			_ = json.Unmarshal(out, &got)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("%s: event round trip differs\n want %s\n  got %s", name, compact(*fx.Event), out)
			}
			continue
		}

		var req Request
		strict(t, name+" request", *fx.Request, &req)
		m, ok := methods[req.Method]
		if !ok {
			t.Errorf("%s: unknown method %q", name, req.Method)
			continue
		}
		if m.params == nil && req.Params != nil {
			t.Errorf("%s: %s takes no params", name, req.Method)
		}
		if m.params != nil && req.Params != nil {
			strict(t, name+" params", req.Params, newOf(m.params))
		}

		var rep Reply
		strict(t, name+" reply", *fx.Reply, &rep)
		if rep.ID != req.ID {
			t.Errorf("%s: reply id %d, request id %d", name, rep.ID, req.ID)
		}
		if (rep.Error == nil) == (rep.Result == nil) {
			t.Errorf("%s: reply needs exactly one of result and error", name)
			continue
		}
		if rep.Error != nil {
			if d, ok := errorData[rep.Error.Code]; ok {
				strict(t, name+" error data", rep.Error.Data, newOf(d))
			} else if rep.Error.Data != nil {
				t.Errorf("%s: %s carries data but has no data type", name, rep.Error.Code)
			}
			continue
		}
		strict(t, name+" result", rep.Result, newOf(m.result))
		if !strings.HasPrefix(name, "error-") {
			covered[req.Method] = true
		}
	}
	for method := range methods {
		if !covered[method] {
			t.Errorf("no success example for method %q", method)
		}
	}
	for _, e := range []string{EventJobStarted, EventProgress, EventJobFinished, EventSourceChanged, EventStateChanged} {
		if !events[e] {
			t.Errorf("no example for event %q", e)
		}
	}
}

// newOf returns a fresh zero value of the same type as v (a pointer).
func newOf(v any) any { return reflect.New(reflect.TypeOf(v).Elem()).Interface() }

func TestUnknownEventDecodesToName(t *testing.T) {
	var e Event
	if err := json.Unmarshal([]byte(`{"event":"from_the_future","x":1}`), &e); err != nil {
		t.Fatal(err)
	}
	if e.Name != "from_the_future" || e.JobStarted != nil || e.Progress != nil {
		t.Errorf("got %+v", e)
	}
}

// An event relayed from a remote carries its host; a local one has none.
func TestEventHost(t *testing.T) {
	e := Event{Name: EventStateChanged, Host: "pantry", StateChanged: &StateChangedEvent{State: StateIdle}}
	b, _ := json.Marshal(e)
	var back Event
	if err := json.Unmarshal(b, &back); err != nil || back.Host != "pantry" || back.StateChanged == nil {
		t.Errorf("round trip %s -> %+v (%v)", b, back, err)
	}
	e.Host = ""
	if b, _ = json.Marshal(e); strings.Contains(string(b), "host") {
		t.Errorf("local event has a host: %s", b)
	}
}
