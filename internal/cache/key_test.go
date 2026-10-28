package cache

import (
	"strings"
	"testing"
)

func TestInspectGolden(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	id, err := Inspect(body)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Cacheable {
		t.Fatal("expected cacheable")
	}
	if id.Model != "m" {
		t.Fatalf("model %q", id.Model)
	}
	blob := canonicalOf(t, body)
	const wantBlob = `{"messages":[{"content":"hi","role":"user"}],"model":"m"}`
	if string(blob) != wantBlob {
		t.Fatalf("canonical\n got %s\nwant %s", blob, wantBlob)
	}
	const wantKey = KeyPrefix + "6d139f1403cd84518768857efed6c375c0e3fe1af3f250cd3a5c59ea87a0323c"
	if id.Key != wantKey {
		t.Fatalf("key %s", id.Key)
	}
}

func TestInspectNormalizesMessages(t *testing.T) {
	variants := []string{
		`{"model":"m","messages":[{"role":"User","content":"  hi  ","name":"  "}]}`,
		`{"messages":[{"content":"hi","role":"user"}],"model":" m "}`,
		`{"model":"m","temperature":0,"stream":false,"messages":[{"role":" user ","content":"hi"}]}`,
	}
	keys := make([]string, len(variants))
	for i, raw := range variants {
		id, err := Inspect([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if !id.Cacheable {
			t.Fatalf("variant %d not cacheable", i)
		}
		keys[i] = id.Key
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] != keys[0] {
			t.Fatalf("key %d differs\n%s\n%s", i, keys[0], keys[i])
		}
	}
}

func TestInspectMultipartTextTrim(t *testing.T) {
	a := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"  hi  "}]}]}`)
	b := []byte(`{"model":"m","messages":[{"role":"User","content":[{"text":"hi","type":"text"}]}]}`)
	ia, err := Inspect(a)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := Inspect(b)
	if err != nil {
		t.Fatal(err)
	}
	if ia.Key != ib.Key {
		t.Fatalf("multipart keys differ\n%s\n%s", ia.Key, ib.Key)
	}
}

func TestInspectPartitionsOnMeaningfulChanges(t *testing.T) {
	base, err := Inspect([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`{"model":"M","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`,
		`{"model":"m","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hey"}],"max_tokens":16}`,
		`{"model":"m","messages":[{"role":"assistant","content":"hey"},{"role":"user","content":"hi"}],"max_tokens":16}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":17}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"n":2}`,
	}
	for _, raw := range cases {
		id, err := Inspect([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if id.Key == base.Key {
			t.Fatalf("expected a different key for %s", raw)
		}
	}
}

func TestInspectTemperatureAndStreamAreEligibilityNotKey(t *testing.T) {
	omitted, err := Inspect([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	zero, err := Inspect([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0}`))
	if err != nil {
		t.Fatal(err)
	}
	hot, err := Inspect([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":1}`))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := Inspect([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Key != zero.Key || omitted.Key != hot.Key || omitted.Key != stream.Key {
		t.Fatalf("temperature/stream leaked into the key: %#v %#v %#v %#v", omitted, zero, hot, stream)
	}
	if !omitted.Cacheable || !zero.Cacheable {
		t.Fatal("omitted and zero temperature must be cacheable")
	}
	if hot.Cacheable || stream.Cacheable {
		t.Fatal("non-zero temperature and stream must bypass the cache")
	}
}

func TestInspectExtrasGolden(t *testing.T) {
	body := []byte(`{"n":1,"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
	blob := canonicalOf(t, body)
	const want = `{"extras":{"max_tokens":16,"n":1},"messages":[{"content":"hi","role":"user"}],"model":"m"}`
	if string(blob) != want {
		t.Fatalf("canonical\n got %s\nwant %s", blob, want)
	}
	id, err := Inspect(body)
	if err != nil {
		t.Fatal(err)
	}
	const wantKey = KeyPrefix + "94898184ae365a844b845cdae0cca1930ff82a599fedf1e3877b74c42c5a0a3a"
	if id.Key != wantKey {
		t.Fatalf("key %s", id.Key)
	}
}

func TestInspectValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"json", `{`, "invalid JSON body"},
		{"trailing", `{"model":"m","messages":[{"role":"user","content":"hi"}]} {"a":1}`, "invalid JSON body"},
		{"array", `[]`, "body must be a JSON object"},
		{"model type", `{"model":1,"messages":[{"role":"user","content":"hi"}]}`, "model must be a string"},
		{"model empty", `{"model":"  ","messages":[{"role":"user","content":"hi"}]}`, "model is required"},
		{"messages missing", `{"model":"m"}`, "messages is required"},
		{"messages empty", `{"model":"m","messages":[]}`, "messages is required"},
		{"messages type", `{"model":"m","messages":{}}`, "messages must be an array"},
		{"message type", `{"model":"m","messages":["hi"]}`, "each message must be an object"},
		{"role", `{"model":"m","messages":[{"content":"hi"}]}`, "message role must be a string"},
		{"role blank", `{"model":"m","messages":[{"role":"  ","content":"hi"}]}`, "message role is required"},
		{"temperature", `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":"hot"}`, "temperature must be a number"},
		{"temperature neg", `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":-0.1}`, "temperature must be a non-negative number"},
		{"stream", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":"yes"}`, "stream must be a boolean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Inspect([]byte(tc.body))
			if err == nil {
				t.Fatal("expected error")
			}
			if !IsValidation(err) {
				t.Fatalf("not validation: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func canonicalOf(t *testing.T, body []byte) []byte {
	t.Helper()
	_, blob, _, err := material(body)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}
