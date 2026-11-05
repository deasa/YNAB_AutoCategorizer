package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/deasa/YNAB_AutoCategorizer/auth"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

var subjectRE = regexp.MustCompile(`^u_[0-9a-f]{12}$`)

// revokeTarget turns the argument into a subject. Anything that is neither an
// email nor a whole subject is refused: hashing a mistyped subject would "revoke"
// nobody and still report success.
func revokeTarget(arg string) (string, error) {
	switch {
	case arg == "--all":
		return auth.AllUsers, nil
	case subjectRE.MatchString(arg):
		return arg, nil
	case strings.HasPrefix(arg, "-") || !strings.Contains(arg, "@"):
		return "", errors.New("expected an email, a u_ hash or --all")
	}
	return auth.UserID(arg), nil
}

// revokeCmd signs a user or everyone out at once. A running server sees it on
// its next request, because tokens check the generation in the database. It
// never prints the email, and needs only FINBOT_DB_PATH.
func revokeCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "finbot revoke: expected one argument\n\n%s", usage)
		return 2
	}
	user, err := revokeTarget(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "finbot revoke: %v\n\n%s", err, usage)
		return 2
	}
	path := config.LoadDBPath()
	// store.Open would create a new, empty database, so a wrong path would report
	// success with every token still valid.
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(stderr, "finbot revoke: no database at FINBOT_DB_PATH; nothing was revoked")
		return 1
	}
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		fmt.Fprintln(stderr, "finbot revoke: open database:", scrub(err.Error(), args[0]))
		return 1
	}
	defer db.Close()
	storage := auth.New(db)
	// Counted first because the revoke invalidates what it counts. A failed count
	// must never get in the way of the revoke, and neither must a zero: a sign-in
	// waiting on the consent page holds no tokens yet and is still cancelled.
	active, countErr := storage.ActiveTokens(ctx, user)
	// auth.Server.RevokeUser does just this, but building a Server needs Google.
	if _, err := storage.BumpGeneration(ctx, user); err != nil {
		fmt.Fprintln(stderr, "finbot revoke:", scrub(err.Error(), args[0]))
		return 1
	}
	who := user
	if user == auth.AllUsers {
		who = "all users"
	}
	switch {
	case countErr != nil:
		fmt.Fprintf(stdout, "revoked tokens for %s (couldn't count them)\n", who)
	case active == 0 && user == auth.AllUsers:
		fmt.Fprintln(stdout, "no active tokens for any user, so nobody was signed out")
	case active == 0:
		fmt.Fprintf(stdout, "no active tokens for %s, so nobody was signed out. If you expected some, check for a typo.\n", who)
	default:
		fmt.Fprintf(stdout, "revoked %d active tokens for %s\n", active, who)
	}
	return 0
}
