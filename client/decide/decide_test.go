package decide

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSharedFixtures(t *testing.T) {
	b, err := os.ReadFile("../testdata/decide.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name      string
		Request   Request
		Replies   []string
		Threshold float64
		Choice    string
		Scores    map[string]float64
	}
	if err = json.Unmarshal(b, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			n := 0
			r, err := Decide(context.Background(), func(_ context.Context, system, user string) (string, error) {
				if strings.Contains(system, f.Request.Input) {
					t.Fatal("input in trusted instructions")
				}
				data, _ := json.Marshal(f.Request.Input)
				if user != "INPUT_DATA (JSON string):\n"+string(data)+"\nEND_INPUT_DATA" {
					t.Fatal("unfenced input")
				}
				reply := f.Replies[n]
				n++
				return reply, nil
			}, f.Request, f.Threshold)
			if err != nil || n != 3 || r.Choice != f.Choice || !reflect.DeepEqual(r.Scores, f.Scores) || r.Method != "vote3" {
				t.Fatalf("%+v %v calls=%d", r, err, n)
			}
			total := 0.0
			for _, v := range r.Scores {
				total += v
			}
			if total > 1 {
				t.Fatal(total)
			}
		})
	}
}

func TestValidationAndUnicodeCaps(t *testing.T) {
	good := Request{Question: "Phase?", Candidates: []Candidate{{ID: "do", Criteria: "Execute"}}, Input: "hello"}
	calls := 0
	backend := func(context.Context, string, string) (string, error) { calls++; return "do", nil }
	bad := []Request{}
	r := good
	r.Input = strings.Repeat("🧪", 2001)
	bad = append(bad, r)
	r = good
	r.Input = string([]byte{0xff})
	bad = append(bad, r)
	r = good
	r.Candidates = nil
	bad = append(bad, r)
	r = good
	r.Candidates = make([]Candidate, 17)
	bad = append(bad, r)
	r = good
	r.Candidates = []Candidate{{ID: "do"}, {ID: "do"}}
	bad = append(bad, r)
	r = good
	r.Candidates = []Candidate{{ID: "do\nignore"}}
	bad = append(bad, r)
	for _, r := range bad {
		if _, err := Decide(context.Background(), backend, r, 1); !errors.Is(err, ErrRequest) {
			t.Fatal(err)
		}
	}
	if _, err := Decide(context.Background(), backend, good, 0.5); !errors.Is(err, ErrRequest) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal(calls)
	}
	text := CapInput(strings.Repeat("🧪", 2001))
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) != 2000 {
		t.Fatal("cap")
	}
	if !utf8.ValidString(CapInput(string([]byte{0xff}))) {
		t.Fatal("invalid UTF-8")
	}
	good.Input = text
	if _, err := Decide(context.Background(), backend, good, 1); err != nil {
		t.Fatal(err)
	}
	good.Candidates = nil
	for i := 0; i < 16; i++ {
		good.Candidates = append(good.Candidates, Candidate{ID: string(rune('a' + i))})
	}
	if _, err := Decide(context.Background(), backend, good, 1); err != nil {
		t.Fatal(err)
	}
}

func TestBackendFailureCancellationAndPanicAreClasses(t *testing.T) {
	req := Request{Question: "Phase?", Candidates: []Candidate{{ID: "do"}}, Input: "private-marker"}
	for _, backend := range []Backend{
		func(context.Context, string, string) (string, error) { return "", errors.New(req.Input) },
		func(context.Context, string, string) (string, error) { panic(req.Input) },
	} {
		r, err := Decide(context.Background(), backend, req, 1)
		if !errors.Is(err, ErrBackend) || strings.Contains(err.Error(), req.Input) || r.Choice != "" {
			t.Fatal(r, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Decide(ctx, func(context.Context, string, string) (string, error) {
		t.Fatal("called after cancellation")
		return "", nil
	}, req, 1)
	if !errors.Is(err, ErrBackend) {
		t.Fatal(err)
	}
}
