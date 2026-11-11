package classify

import (
	"strings"
	"unicode"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// P2P detection (§3.2): most P2P payees don't contain "Venmo", so a substring
// match is both too narrow and too wide. Instead:
//
//   - The payee is a P2P service's bank descriptor, matched on whole words:
//     "Zelle Payment To …", "CASH APP*…", "PAYPAL *INST XFER", "Apple Cash …".
//     "Zelle's Pizza", "Cash Wise Foods" and "PAYPAL *NETFLIX" are merchants.
//   - Or the payee looks like a person's name and there's a P2P signal around
//     it: the account is a P2P wallet (named Venmo, PayPal, …), or the memo
//     names a P2P service, reads like a P2P note, or is only emoji.

// p2pServices are one-word service names, matched as whole words.
var p2pServices = wordSet("venmo", "zelle", "cashapp", "popmoney")

// p2pPhrases are multi-word service names, matched as whole-word runs.
var p2pPhrases = []string{"cash app", "square cash", "sq cash", "apple cash"}

// paypalTransferWords turn "paypal" into a P2P transfer; without one, PayPal is
// a way to pay a merchant.
var paypalTransferWords = wordSet("inst", "xfer", "transfer", "instant", "send", "sent", "p2p")

// p2pMemoPhrases read like the note on a P2P payment.
var p2pMemoPhrases = []string{"sent money", "paid you", "you paid", "payment from", "payment to", "request from", "paid back", "pay back"}

// merchantWords rule out a person-shaped payee: "Mountain West Pizza".
var merchantWords = wordSet(
	"inc", "llc", "ltd", "co", "corp", "company", "the", "and", "of",
	"store", "stores", "shop", "market", "mart", "foods", "food", "grocery", "pharmacy",
	"cafe", "coffee", "pizza", "grill", "restaurant", "kitchen", "bakery", "bar", "pub", "diner", "tacos", "burger",
	"service", "services", "school", "church", "center", "clinic", "dental", "medical", "salon", "spa",
	"auto", "hardware", "farms", "farm", "park", "club", "studio", "academy", "house", "home",
	"bank", "insurance", "utilities", "power", "energy", "gas", "water", "city", "county", "state",
	"online", "com", "app", "pay", "payment", "transfer", "deposit", "refund",
)

func isP2P(r Row) bool {
	d := r.Detail
	payee := PayeeName(d)
	if isP2PDescriptor(store.NormalizePayee(payee)) {
		return true
	}
	if !personShaped(payee) {
		return false
	}
	account := ""
	if d.AccountName != nil {
		account = *d.AccountName
	}
	memo := ""
	if d.Memo != nil {
		memo = *d.Memo
	}
	return isP2PAccount(account) || isP2PMemo(memo)
}

// isP2PAccount reports whether an account is a P2P wallet: "Venmo", "PayPal
// Balance", "Cash App".
func isP2PAccount(name string) bool {
	norm := store.NormalizePayee(name)
	return isP2PDescriptor(norm) || hasPhrase(norm, []string{"paypal"})
}

// isP2PDescriptor reports whether a normalized name names a P2P service.
func isP2PDescriptor(norm string) bool {
	words := strings.Fields(norm)
	if hasAny(words, p2pServices) {
		return true
	}
	if hasPhrase(norm, p2pPhrases) {
		return true
	}
	for i, w := range words {
		if w != "paypal" {
			continue
		}
		if hasAny(words[i+1:], paypalTransferWords) {
			return true
		}
	}
	return false
}

// isP2PMemo reports whether a memo carries a P2P signal.
func isP2PMemo(memo string) bool {
	memo = strings.TrimSpace(memo)
	if memo == "" {
		return false
	}
	norm := store.NormalizePayee(memo)
	if norm == "" {
		return true // only emoji or symbols: the Venmo-note style
	}
	if isP2PAccount(norm) {
		return true
	}
	return hasPhrase(norm, p2pMemoPhrases)
}

// hasPhrase reports whether norm holds one of phrases as a run of whole words.
func hasPhrase(norm string, phrases []string) bool {
	padded := " " + norm + " "
	for _, p := range phrases {
		if strings.Contains(padded, " "+p+" ") {
			return true
		}
	}
	return false
}

// personShaped reports whether a payee looks like a person's name: two or three
// words of letters, each capitalized, none a merchant word. "Jane Doe" and
// "JOHN SMITH" are; "Target" and "Mountain West Pizza" aren't.
func personShaped(payee string) bool {
	words := strings.Fields(payee)
	if len(words) < 2 || len(words) > 3 {
		return false
	}
	for _, w := range words {
		w = strings.TrimRight(w, ".")
		rs := []rune(w)
		if len(rs) == 0 || !unicode.IsUpper(rs[0]) {
			return false
		}
		for _, r := range rs {
			if !unicode.IsLetter(r) && r != '\'' && r != '’' && r != '-' {
				return false
			}
		}
		if merchantWords[store.NormalizePayee(w)] {
			return false
		}
	}
	return true
}
