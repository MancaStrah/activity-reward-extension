package utils

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
)

// fakeRegistry is a registry whose answers — and whose failures — are scripted.
//
// The failures are the point. ResolveExtensionId's contract is "refuse to guess when
// the answer is ambiguous", and that is only true if an id it could not READ is
// distinguished from an id that did not MATCH. Nothing could pin that before, because
// the generated binding is a concrete type; this fake is the seam.
type fakeRegistry struct {
	count    int64
	countErr error
	// senders maps an extension id to the instruction sender registered for it.
	senders map[int64]common.Address
	// errOn makes the lookup of these ids fail, as a flaky or rate-limited RPC would.
	errOn map[int64]bool
	// reads records every id actually looked up, so a test can assert how far the
	// scan got rather than only what it returned.
	reads []int64
}

func (f *fakeRegistry) NextPublicExtensionId(_ *bind.CallOpts) (*big.Int, error) {
	if f.countErr != nil {
		return nil, f.countErr
	}
	return big.NewInt(f.count), nil
}

func (f *fakeRegistry) GetTeeExtensionInstructionsSender(_ *bind.CallOpts, id *big.Int) (common.Address, error) {
	f.reads = append(f.reads, id.Int64())
	if f.errOn[id.Int64()] {
		return common.Address{}, errors.New("rpc: rate limit exceeded")
	}
	return f.senders[id.Int64()], nil
}

var (
	ours  = common.HexToAddress("0x00000000000000000000000000000000000000AA")
	other = common.HexToAddress("0x00000000000000000000000000000000000000BB")
)

const firstPublic = int64(firstPublicExtensionID)

func resolve(t *testing.T, reg *fakeRegistry) (*big.Int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return ResolveExtensionIDWithRegistry(ctx, reg, ours)
}

// TestResolveExtensionIDRefusesWhenAnIDCouldNotBeRead is the regression test for the
// defect this seam was added to catch.
//
// The attacker's registration sits at the lower id and the owner's at the higher one.
// The lookup of the attacker's id fails. Skipping it — which the scan used to do —
// leaves exactly one match, and "exactly one match" is what the caller feeds into
// setExtensionId: owner-only, one-shot, and satisfied on-chain by either id, because
// the registry maps both to this contract. So a single transient RPC error would have
// turned "ambiguous, refuse" into "unique, bind permanently".
func TestResolveExtensionIDRefusesWhenAnIDCouldNotBeRead(t *testing.T) {
	reg := &fakeRegistry{
		count: firstPublic + 3,
		senders: map[int64]common.Address{
			firstPublic:     ours, // the attacker's pre-registration
			firstPublic + 1: other,
			firstPublic + 2: ours, // the owner's registration
		},
		errOn: map[int64]bool{firstPublic: true},
	}

	id, err := resolve(t, reg)
	if err == nil {
		t.Fatalf("an unreadable id must not be reported as unambiguous, got id %v", id)
	}
	if id != nil {
		t.Fatalf("no id may be returned alongside the refusal, got %v", id)
	}
	// The message has to name what happened, because the operator's next move is to
	// pass the id explicitly rather than retry blindly.
	for _, want := range []string{"refusing", "explicitly"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
	// And it must stop at the failure rather than scan on and report on a partial view.
	if len(reg.reads) != 1 || reg.reads[0] != firstPublic {
		t.Errorf("the scan should stop at the unreadable id, read %v", reg.reads)
	}
}

// TestResolveExtensionIDRefusesADuplicate is the case that must keep working: two
// genuine matches, both readable.
func TestResolveExtensionIDRefusesADuplicate(t *testing.T) {
	reg := &fakeRegistry{
		count: firstPublic + 3,
		senders: map[int64]common.Address{
			firstPublic:     ours,
			firstPublic + 1: other,
			firstPublic + 2: ours,
		},
	}
	id, err := resolve(t, reg)
	if err == nil {
		t.Fatalf("two registrations of the same address must be refused, got id %v", id)
	}
	if !strings.Contains(err.Error(), "multiple extensions") {
		t.Errorf("the refusal should name the duplication: %v", err)
	}
}

// TestResolveExtensionIDReturnsTheOnlyMatch guards the accepting direction, so the
// refusals above are not just "it refuses everything".
func TestResolveExtensionIDReturnsTheOnlyMatch(t *testing.T) {
	reg := &fakeRegistry{
		count: firstPublic + 3,
		senders: map[int64]common.Address{
			firstPublic:     other,
			firstPublic + 1: ours,
			firstPublic + 2: other,
		},
	}
	id, err := resolve(t, reg)
	if err != nil {
		t.Fatalf("a single readable match must resolve: %v", err)
	}
	if id.Int64() != firstPublic+1 {
		t.Fatalf("resolved the wrong id: got %v, want %d", id, firstPublic+1)
	}
	// Every public id must have been read — a scan that stopped at the first match
	// could not have detected a second one.
	if len(reg.reads) != 3 {
		t.Errorf("the whole public id range must be scanned, read %v", reg.reads)
	}
}

// TestResolveExtensionIDReportsNoMatch keeps the never-registered case distinct from
// the ambiguous one; they need different advice.
func TestResolveExtensionIDReportsNoMatch(t *testing.T) {
	reg := &fakeRegistry{
		count:   firstPublic + 2,
		senders: map[int64]common.Address{firstPublic: other, firstPublic + 1: other},
	}
	if _, err := resolve(t, reg); err == nil || !strings.Contains(err.Error(), "no extension registers") {
		t.Fatalf("an unregistered address should say so plainly: %v", err)
	}
}

// TestResolveExtensionIDRefusesAnUnfinishedScan pins the other way the scan can come
// up short: the budget runs out partway. Absorbing that reads as "nothing else
// matched", which is the same wrong answer as swallowing a per-id error.
func TestResolveExtensionIDRefusesAnUnfinishedScan(t *testing.T) {
	reg := &fakeRegistry{
		count:   firstPublic + 3,
		senders: map[int64]common.Address{firstPublic: other, firstPublic + 1: ours, firstPublic + 2: ours},
	}
	// Already expired: the loop must notice before it reads anything and reports.
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	id, err := ResolveExtensionIDWithRegistry(ctx, reg, ours)
	if err == nil {
		t.Fatalf("an unfinished scan must not yield an id, got %v", id)
	}
	if !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("the refusal should say the scan did not finish: %v", err)
	}
	if len(reg.reads) != 0 {
		t.Errorf("nothing should be read once the budget is gone, read %v", reg.reads)
	}
}

// TestResolveExtensionIDPropagatesACountFailure — without the count there is no range
// to scan, so this cannot be treated as "zero matches".
func TestResolveExtensionIDPropagatesACountFailure(t *testing.T) {
	reg := &fakeRegistry{countErr: errors.New("rpc: connection reset")}
	if _, err := resolve(t, reg); err == nil || !strings.Contains(err.Error(), "nextPublicExtensionId") {
		t.Fatalf("a failed count must be reported, not read as an empty registry: %v", err)
	}
}
