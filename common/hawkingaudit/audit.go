// Package hawkingaudit records token-accounting differences between the
// pre-Hawking execution used for historical replay and the 1.6.0 fixes.
// It never changes chain state. Full sync is single-threaded, but writes are
// still serialized so a parallel test cannot corrupt the file.
package hawkingaudit

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"sync"

	"github.com/PlatONnetwork/PlatON-Go/common"
	"github.com/PlatONnetwork/PlatON-Go/log"
)

// AuditPath is the append-only report written during a full historical replay.
const AuditPath = "/opt/node/data/platon/hawking_token_audit.log"

// AuditStatePath persists the in-memory tracker so a restarted full sync
// does not treat later payments as unrelated to earlier deposits.
const AuditStatePath = AuditPath + ".state"

// Event is one detected difference. Amounts are in von (1 LAT = 1e18 von).
// ContractShortfall is how much the built-in contract paid out too much.
// TopUp is how much must be added to that contract before it is safe.
// UserShortfall is value that stayed in the contract and should be returned
// to Account; TopUp is zero in that case because the contract is not short.
type Event struct {
	Kind              string `json:"kind"`
	BlockNumber       uint64 `json:"blockNumber"`
	BlockHash         string `json:"blockHash"`
	TxHash            string `json:"txHash"`
	Account           string `json:"account,omitempty"`
	PaidTo            string `json:"paidTo"`
	WrongAmount       string `json:"wrongAmount"`
	Contract          string `json:"contract"`
	ContractShortfall string `json:"contractShortfall"`
	TopUp             string `json:"topUp"`
	UserShortfall     string `json:"userShortfall"`
	Note              string `json:"note"`
}

var (
	mu       sync.Mutex
	file     *os.File
	disabled bool
	accounts = map[common.Address]*acctTrack{}
	loaded   bool
	persist  = true
	curBlock uint64
	curHash  common.Hash
)

type acctTrack struct {
	// unbacked is restricting value already paid and still sitting in a release plan.
	unbacked *big.Int
	// backed is principal added to Cache by that same deposit and not yet released.
	backed *big.Int
	// stuck is the principal-stuck amount already written for this account.
	stuck *big.Int
}

func trackOf(account common.Address) *acctTrack {
	t := accounts[account]
	if t == nil {
		t = &acctTrack{unbacked: new(big.Int), backed: new(big.Int), stuck: new(big.Int)}
		accounts[account] = t
	}
	return t
}

// SetBlock records the block currently being executed so host calls can attach its hash.
func SetBlock(number uint64, hash common.Hash) {
	mu.Lock()
	curBlock = number
	curHash = hash
	mu.Unlock()
}

// CurrentBlockHash returns the hash last passed to SetBlock.
func CurrentBlockHash() common.Hash {
	mu.Lock()
	defer mu.Unlock()
	return curHash
}

// NoteDeposit tracks a pre-Hawking deposit that paid debt and still scheduled the full plan.
// unbackedAmt was already sent to the user. backedAmt was added to Cache.
func NoteDeposit(account common.Address, unbackedAmt, backedAmt *big.Int) {
	if (unbackedAmt == nil || unbackedAmt.Sign() <= 0) && (backedAmt == nil || backedAmt.Sign() <= 0) {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	ensureLoadedLocked()
	t := trackOf(account)
	if unbackedAmt != nil && unbackedAmt.Sign() > 0 {
		t.unbacked.Add(t.unbacked, unbackedAmt)
	}
	if backedAmt != nil && backedAmt.Sign() > 0 {
		t.backed.Add(t.backed, backedAmt)
	}
	saveLocked()
}

// RealizeDebtPayment is cash that pays NeedRelease. Only the unbacked part is an extra payment.
func RealizeDebtPayment(account common.Address, paid *big.Int) *big.Int {
	if paid == nil || paid.Sign() <= 0 {
		return new(big.Int)
	}
	mu.Lock()
	defer mu.Unlock()
	ensureLoadedLocked()
	t := accounts[account]
	if t == nil || t.unbacked.Sign() <= 0 {
		return new(big.Int)
	}
	over := minBig(paid, t.unbacked)
	if over.Sign() == 0 {
		return over
	}
	t.unbacked.Sub(t.unbacked, over)
	saveLocked()
	return over
}

// RealizeCacheRelease is cash paid from Cache at epoch release.
// Backed principal is consumed first. Only the remainder, up to unbacked, was paid twice.
func RealizeCacheRelease(account common.Address, paid *big.Int) *big.Int {
	if paid == nil || paid.Sign() <= 0 {
		return new(big.Int)
	}
	mu.Lock()
	defer mu.Unlock()
	ensureLoadedLocked()
	t := accounts[account]
	if t == nil || (t.backed.Sign() <= 0 && t.unbacked.Sign() <= 0) {
		return new(big.Int)
	}
	legit := minBig(paid, t.backed)
	t.backed.Sub(t.backed, legit)
	rest := new(big.Int).Sub(paid, legit)
	over := minBig(rest, t.unbacked)
	t.unbacked.Sub(t.unbacked, over)
	if legit.Sign() > 0 || over.Sign() > 0 {
		saveLocked()
	}
	return over
}

// StuckIncrease returns how much newly stuck principal has not been reported yet.
// A later call with the same or a smaller balance returns zero.
func StuckIncrease(account common.Address, now *big.Int) *big.Int {
	mu.Lock()
	defer mu.Unlock()
	ensureLoadedLocked()
	if now == nil || now.Sign() < 0 {
		now = new(big.Int)
	}
	t := accounts[account]
	if t == nil {
		if now.Sign() == 0 {
			return new(big.Int)
		}
		t = trackOf(account)
	}
	if now.Cmp(t.stuck) <= 0 {
		if now.Cmp(t.stuck) < 0 {
			t.stuck.Set(now)
			saveLocked()
		}
		return new(big.Int)
	}
	delta := new(big.Int).Sub(now, t.stuck)
	t.stuck.Set(now)
	saveLocked()
	return delta
}

func minBig(a, b *big.Int) *big.Int {
	if a == nil || b == nil || a.Sign() <= 0 || b.Sign() <= 0 {
		return new(big.Int)
	}
	if a.Cmp(b) <= 0 {
		return new(big.Int).Set(a)
	}
	return new(big.Int).Set(b)
}

type persistedAcct struct {
	Unbacked string `json:"unbacked"`
	Backed   string `json:"backed"`
	Stuck    string `json:"stuck"`
}

func ensureLoadedLocked() {
	if loaded {
		return
	}
	loaded = true
	raw, err := os.ReadFile(AuditStatePath)
	if err != nil {
		return
	}
	var snap map[string]persistedAcct
	if err := json.Unmarshal(raw, &snap); err != nil {
		log.Error("hawking token audit state ignored", "path", AuditStatePath, "err", err)
		return
	}
	for key, item := range snap {
		t := trackOf(common.HexToAddress(key))
		t.unbacked.Set(parseAmount(item.Unbacked))
		t.backed.Set(parseAmount(item.Backed))
		t.stuck.Set(parseAmount(item.Stuck))
	}
}

func saveLocked() {
	if !persist {
		return
	}
	snap := make(map[string]persistedAcct, len(accounts))
	for account, t := range accounts {
		if t.unbacked.Sign() == 0 && t.backed.Sign() == 0 && t.stuck.Sign() == 0 {
			continue
		}
		snap[account.Hex()] = persistedAcct{
			Unbacked: t.unbacked.String(),
			Backed:   t.backed.String(),
			Stuck:    t.stuck.String(),
		}
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Error("hawking token audit state encode failed", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(AuditStatePath), 0o755); err != nil {
		log.Error("hawking token audit state save failed", "path", AuditStatePath, "err", err)
		return
	}
	tmp := AuditStatePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Error("hawking token audit state save failed", "path", AuditStatePath, "err", err)
		return
	}
	if err := os.Rename(tmp, AuditStatePath); err != nil {
		log.Error("hawking token audit state save failed", "path", AuditStatePath, "err", err)
	}
}

func parseAmount(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return n
}

func resetForTest() {
	mu.Lock()
	accounts = map[common.Address]*acctTrack{}
	loaded = true
	persist = false
	mu.Unlock()
}

// Write appends one event. A failure to open the file is logged once and then
// ignored so a full sync is not aborted.
func Write(e Event) {
	mu.Lock()
	defer mu.Unlock()
	if disabled {
		return
	}
	if file == nil {
		ensureLoadedLocked()
		if err := os.MkdirAll(filepath.Dir(AuditPath), 0o755); err != nil {
			disabled = true
			log.Error("hawking token audit disabled", "path", AuditPath, "err", err)
			return
		}
		f, err := os.OpenFile(AuditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			disabled = true
			log.Error("hawking token audit disabled", "path", AuditPath, "err", err)
			return
		}
		file = f
	}
	enc := json.NewEncoder(file)
	if err := enc.Encode(e); err != nil {
		log.Error("hawking token audit write failed", "err", err)
		return
	}
	if err := file.Sync(); err != nil {
		log.Error("hawking token audit sync failed", "err", err)
	}
}

func addr(a common.Address) string { return a.Hex() }

func noteWithTx(txHash common.Hash, note string) string {
	if txHash == (common.Hash{}) {
		return note + "; epoch release is not a user transaction, txHash is empty, use blockHash"
	}
	return note
}

func hash(h common.Hash) string { return h.Hex() }

func amt(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

// RecordRestrictingOverpay logs a realized extra payment out of the restricting contract.
func RecordRestrictingOverpay(blockNumber uint64, blockHash, txHash common.Hash, account common.Address, contract common.Address, wrong *big.Int) {
	if wrong == nil || wrong.Sign() <= 0 {
		return
	}
	Write(Event{
		Kind:              "restricting-overpay",
		BlockNumber:       blockNumber,
		BlockHash:         hash(blockHash),
		TxHash:            hash(txHash),
		Account:           addr(account),
		PaidTo:            addr(account),
		WrongAmount:       amt(wrong),
		Contract:          addr(contract),
		ContractShortfall: amt(wrong),
		TopUp:             amt(wrong),
		UserShortfall:     "0",
		Note:              noteWithTx(txHash, "debt already paid was released again; restricting contract balance fell by wrongAmount; top up that amount"),
	})
}

// RecordStuckPrincipal logs principal left in the restricting contract because no release epoch remained.
func RecordStuckPrincipal(blockNumber uint64, blockHash, txHash common.Hash, account common.Address, contract common.Address, stuck *big.Int) {
	if stuck == nil || stuck.Sign() <= 0 {
		return
	}
	Write(Event{
		Kind:              "restricting-principal-stuck",
		BlockNumber:       blockNumber,
		BlockHash:         hash(blockHash),
		TxHash:            hash(txHash),
		Account:           addr(account),
		PaidTo:            addr(account),
		WrongAmount:       amt(stuck),
		Contract:          addr(contract),
		ContractShortfall: "0",
		TopUp:             "0",
		UserShortfall:     amt(stuck),
		Note:              noteWithTx(txHash, "principal remains in the restricting contract; the contract is not short; pay wrongAmount from the contract to account"),
	})
}

// RecordStaticTransfer logs a platon_transfer that succeeded inside a read-only frame.
func RecordStaticTransfer(blockNumber uint64, blockHash, txHash common.Hash, from, to common.Address, value *big.Int) {
	if value == nil || value.Sign() <= 0 {
		return
	}
	if blockHash == (common.Hash{}) {
		blockHash = CurrentBlockHash()
	}
	Write(Event{
		Kind:              "static-platon-transfer",
		BlockNumber:       blockNumber,
		BlockHash:         hash(blockHash),
		TxHash:            hash(txHash),
		Account:           addr(from),
		PaidTo:            addr(to),
		WrongAmount:       amt(value),
		Contract:          addr(from),
		ContractShortfall: amt(value),
		TopUp:             amt(value),
		UserShortfall:     "0",
		Note:              "platon_transfer in a read-only frame was not rejected; value moved from account to paidTo and is kept because STATICCALL only reverts when the call returns an error",
	})
}
