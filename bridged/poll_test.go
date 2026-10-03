package bridged

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-bridge/rate"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
)

// serving is a source that answers with a fixed body and status.
func serving(t *testing.T, status int, body string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(srv.Close)

	return srv.URL
}

func TestFetchPrice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		field  string
		want   float64
		ok     bool
	}{
		{
			name: "a number", status: 200, body: `{"price": 0.00308078}`,
			field: "price", want: 0.00308078, ok: true,
		},
		{
			name: "a nested number", status: 200,
			body:  `{"data": {"last": 0.0031}}`,
			field: "data.last", want: 0.0031, ok: true,
		},
		{
			// Venues quote numbers as strings often enough that refusing
			// them would mean refusing the venue.
			name: "a quoted number", status: 200, body: `{"price": "0.0031"}`,
			field: "price", want: 0.0031, ok: true,
		},
		{
			name: "not found", status: 404, body: `nope`, field: "price",
		},
		{
			name: "not JSON", status: 200, body: `<html>`, field: "price",
		},
		{
			name: "missing field", status: 200, body: `{"other": 1}`,
			field: "price",
		},
		{
			name: "not a number", status: 200, body: `{"price": "abc"}`,
			field: "price",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := fetchPrice(context.Background(), RateSource{
				Name: "test", URL: serving(t, test.status, test.body),
				Field: test.field,
			})
			if !test.ok {
				if err == nil {
					t.Fatalf("wanted an error, got %v", got)
				}

				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Errorf("got %v, wanted %v", got, test.want)
			}
		})
	}
}

// A body that never ends must not take the daemon with it.
func TestFetchPriceIsBounded(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			// More than the reader allows, and not valid JSON either way.
			for i := 0; i < 64; i++ {
				_, _ = w.Write([]byte(strings.Repeat("x", 1<<16)))
			}
		}))
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = fetchPrice(context.Background(), RateSource{
			Name: "flood", URL: srv.URL, Field: "price",
		})
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("fetchPrice did not give up on an endless body")
	}
}

// A source that fails is left out rather than defaulted. The oracle's whole
// job is to notice when sources disagree, and a made-up number that agrees
// with everything defeats it.
func TestSamplePriceDropsAFailedSourceRatherThanInventingOne(t *testing.T) {
	t.Parallel()

	good := serving(t, 200, `{"price": 0.0031}`)
	bad := serving(t, 500, `down`)

	oracle, err := rate.New(rate.DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}

	s := &Service{log: quiet(), oracle: oracle, cfg: Config{RateSources: []RateSource{
		{Name: "good", URL: good, Field: "price"},
		{Name: "bad", URL: bad, Field: "price"},
	}}}
	s.priceErr = fmt.Errorf("none yet")

	s.samplePrice(context.Background())

	s.priceMu.RLock()
	got, gotErr := s.price, s.priceErr
	s.priceMu.RUnlock()

	if gotErr != nil {
		t.Fatalf("one good source should still price: %v", gotErr)
	}
	if got.Sources != 1 {
		t.Errorf("counted %d sources, wanted only the one that answered",
			got.Sources)
	}
	if got.OutgoingPerIncoming != 0.0031 {
		t.Errorf("priced at %v, wanted the good source's %v",
			got.OutgoingPerIncoming, 0.0031)
	}
}

// With every source down the daemon must refuse to quote, not carry on with
// the last price it happened to see.
func TestSamplePriceRefusesWhenEverySourceFails(t *testing.T) {
	t.Parallel()

	oracle, err := rate.New(rate.DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}

	s := &Service{log: quiet(), oracle: oracle, cfg: Config{RateSources: []RateSource{
		{Name: "bad", URL: serving(t, 500, `down`), Field: "price"},
	}}}
	// A price from before, which must not be served once the sources are
	// gone.
	s.price = rate.Reading{OutgoingPerIncoming: 0.0031, At: time.Now()}
	s.priceErr = nil

	s.samplePrice(context.Background())

	s.priceMu.RLock()
	gotErr := s.priceErr
	s.priceMu.RUnlock()

	if gotErr == nil {
		t.Fatal("no usable source should leave the daemon refusing to price")
	}

	// And the refusal has to reach the quote, not just the field.
	sd := &side{name: "toBitcoin", balance: func(context.Context) (uint64, error) {
		return 1, nil
	}}
	if _, err := s.pricer(sd, inventoryForTest(), sd.balance)(context.Background()); err == nil {
		t.Error("a quote should be refused while there is no usable price")
	}
}

// resume has to send each swap to the direction whose paying node can read
// its invoice. Running both directions' Resume over one journal would have
// each drive every swap, including the one whose nodes are reversed.
func TestResumeRoutesEachSwapToItsOwnDirection(t *testing.T) {
	t.Parallel()

	j := openJournal(t, t.TempDir())
	defer j.Close()

	btc := &fakeOut{accepts: "ln"}
	lf := &fakeOut{accepts: "ln"}
	s := testService(t, btc, lf, nil)
	s.journal = j

	// Two unfinished swaps, one for each direction.
	now := time.Now()
	for i, invoice := range []string{"lnbcrt1payme", "lnbcrt1" + b2bMarker + "payme"} {
		err := j.Put(context.Background(), store.Record{
			Hash: node8(byte(i + 1)), State: swap.Offered,
			OutgoingCLTVLimit: 40, Invoice: invoice,
			IncomingMsat: 1_000_000, OutgoingMsat: 3_080,
			Rate: 0.00308078, Spread: 0.01, Created: now, Updated: now,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Runners are nil here on purpose: this is about routing, and driving
	// is covered by the live tests. Count the decode calls instead.
	btcBefore, lfBefore := btc.calls, lf.calls

	pending, err := j.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var routed []string
	for _, rec := range pending {
		sd, _, err := s.route(context.Background(), rec.Invoice)
		if err != nil {
			t.Fatalf("%s: %v", rec.Invoice, err)
		}
		routed = append(routed, sd.name)
	}

	if len(routed) != 2 {
		t.Fatalf("routed %d swaps, wanted 2", len(routed))
	}
	seen := map[string]bool{routed[0]: true, routed[1]: true}
	if !seen["toBitcoin"] || !seen["toBlake2b"] {
		t.Errorf("both directions should have been used, got %v", routed)
	}
	if btc.calls == btcBefore && lf.calls == lfBefore {
		t.Error("routing did not ask either node to decode anything")
	}
}

// A swap whose direction is no longer enabled must be reported loudly: it is
// an HTLC nobody is watching, which is the worst way to be quiet.
func TestResumeReportsASwapItCannotRoute(t *testing.T) {
	t.Parallel()

	j := openJournal(t, t.TempDir())
	defer j.Close()

	// Only the Bitcoin direction is enabled, and the journal holds a swap
	// for the other one.
	s := testService(t, &fakeOut{accepts: "lnbc"}, nil, nil)
	s.journal = j

	now := time.Now()
	err := j.Put(context.Background(), store.Record{
		Hash: node8(9), State: swap.Funded, OutgoingCLTVLimit: 40,
		Invoice: "lnblakert1orphan", IncomingMsat: 1_000_000,
		OutgoingMsat: 3_080, Rate: 0.00308078, Spread: 0.01,
		Created: now, Updated: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.resume(context.Background())
	if err == nil {
		t.Fatal("a swap that cannot be routed should be reported")
	}
	if !strings.Contains(err.Error(), "lnblakert1orphan") &&
		!strings.Contains(err.Error(), fmt.Sprintf("%x", node8(9))) {

		t.Errorf("the error should identify the stuck swap: %v", err)
	}
}

func TestResumeWithNothingPendingIsQuiet(t *testing.T) {
	t.Parallel()

	j := openJournal(t, t.TempDir())
	defer j.Close()

	s := testService(t, &fakeOut{accepts: "lnbc"}, nil, nil)
	s.journal = j

	if err := s.resume(context.Background()); err != nil {
		t.Errorf("an empty journal should resume cleanly: %v", err)
	}
}
