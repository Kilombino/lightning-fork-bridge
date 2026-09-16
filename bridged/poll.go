package bridged

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/paulscode/lightning-fork-bridge/chainrate"
	"github.com/paulscode/lightning-fork-bridge/lndrpc/lnrpc"
	"github.com/paulscode/lightning-fork-bridge/rate"
)

// pollChains keeps both chain observers fed until ctx ends.
//
// A chain observer with nothing in it refuses to estimate, and a refused
// estimate refuses the quote that needed it. That is the right way round: the
// bridge cannot size an HTLC against a chain it has not looked at.
func (s *Service) pollChains(ctx context.Context) {
	tick := time.NewTicker(s.cfg.pollInterval())
	defer tick.Stop()

	for {
		s.sampleChain(ctx, s.lfConn, s.lfChain, "blake2b")
		s.sampleChain(ctx, s.btcConn, s.btcChain, "bitcoin")

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// sampleChain adds the tip to an observer.
func (s *Service) sampleChain(ctx context.Context, conn *grpc.ClientConn,
	obs *chainrate.Observer, which string) {

	// A poller that panics takes the daemon down with it, including any
	// swap holding an HTLC, and it would do that for a reason unrelated to
	// the swap. New always sets both connections, so this is a programming
	// error rather than an operational one, but it should not be a fatal
	// one from a background goroutine.
	if conn == nil {
		s.log.Warn("no connection to sample from", "chain", which)

		return
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	info, err := lnrpc.NewLightningClient(conn).GetInfo(ctx,
		&lnrpc.GetInfoRequest{})
	if err != nil {
		s.log.Warn("could not read the chain tip", "chain", which,
			"err", err)

		return
	}

	// BestHeaderTimestamp is when the tip was mined, which is the quantity
	// the spacing is measured from. The local clock says when this node
	// heard about it, which is a different number and not the one wanted.
	b := chainrate.Block{
		Height: int32(info.GetBlockHeight()),
		Time:   time.Unix(info.GetBestHeaderTimestamp(), 0),
	}
	if b.Height == 0 || b.Time.IsZero() {
		return
	}
	// Add rejects a height it already holds, which is the common case
	// between blocks and is not worth logging.
	_ = obs.Add(b)
}

// pollPrice keeps the combined rate reading fresh until ctx ends.
func (s *Service) pollPrice(ctx context.Context) {
	tick := time.NewTicker(s.cfg.pollInterval())
	defer tick.Stop()

	for {
		s.samplePrice(ctx)

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// samplePrice reads every source and combines them.
//
// A source that fails is left out rather than defaulted: the oracle's whole
// job is to notice when sources disagree, and a made-up number that agrees
// with everything defeats it. If that leaves too few, the oracle refuses and
// the refusal is what quotes see.
func (s *Service) samplePrice(ctx context.Context) {
	// Same reasoning as sampleChain: New always builds the oracle, so a
	// missing one is a programming error, and a background goroutine is
	// the wrong place to turn one into a dead daemon.
	if s.oracle == nil {
		s.log.Warn("no rate oracle to price with")

		return
	}

	obs := make([]rate.Observation, 0, len(s.cfg.RateSources))
	for _, src := range s.cfg.RateSources {
		price, err := fetchPrice(ctx, src)
		if err != nil {
			s.log.Warn("a rate source failed", "source", src.Name,
				"err", err)

			continue
		}
		obs = append(obs, rate.Observation{
			Source: src.Name, OutgoingPerIncoming: price, At: time.Now(),
		})
	}

	reading, err := s.oracle.Update(time.Now(), obs)

	s.priceMu.Lock()
	if err != nil {
		s.priceErr = err
	} else {
		s.price, s.priceErr = reading, nil
	}
	s.priceMu.Unlock()

	if err != nil {
		s.log.Warn("no usable price", "sources", len(obs), "err", err)

		return
	}
	s.log.Info("price", "outgoingPerIncoming", reading.OutgoingPerIncoming,
		"sources", reading.Sources, "spread", reading.Spread)
}

// priceClient fetches prices. Redirects are refused rather than followed: the
// URL is the operator's choice, and a source that answers with a redirect has
// stopped being the thing they configured.
var priceClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// fetchPrice reads one source.
func fetchPrice(ctx context.Context, src RateSource) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := priceClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s answered %s", src.URL, resp.Status)
	}

	// Bounded: a source that streams forever must not take the daemon with
	// it, and a price is a few hundred bytes.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}

	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, fmt.Errorf("%s did not return JSON: %w", src.URL, err)
	}

	return extract(doc, src.Field)
}

// extract walks a dotted path and reads a number, which JSON may have encoded
// as a string.
func extract(doc any, path string) (float64, error) {
	cur := doc
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("%q: %s is not an object", path, part)
		}
		cur, ok = obj[part]
		if !ok {
			return 0, fmt.Errorf("%q: no field %s", path, part)
		}
	}

	switch v := cur.(type) {
	case float64:
		return v, nil
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("%q holds %q, which is not a number", path, v)
		}

		return f, nil
	default:
		return 0, fmt.Errorf("%q holds %T, which is not a number", path, cur)
	}
}
