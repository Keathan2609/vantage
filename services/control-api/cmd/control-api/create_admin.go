package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/vantage/control-api/internal/app"
	"github.com/vantage/control-api/internal/auth"
	"github.com/vantage/control-api/internal/domain"
)

// runCreateAdmin creates the first administrator in a deployed environment.
//
// Without this the platform cannot be bootstrapped anywhere but development.
// `seed` is the only other caller of CreateUser and it refuses outside
// development and test, correctly, because it writes accounts whose passwords
// are published in this repository. The result was a guard with no legitimate
// path behind it: deploy to production, then discover there is no way to log
// in and no command that can make one.
//
// The password is READ FROM THE OPERATOR, never generated and never defaulted.
// A command that printed a password it had chosen would put that password into
// a terminal scrollback, a CI log and a shell history, and an operator who has
// to be told their own credential has one they did not choose.
//
// It is read from a TTY without echo where there is one, so it does not appear
// on screen. Where there is no TTY it is read from stdin, which is what makes
// `echo "..." | control-api create-admin` work for an automated first boot
// without the password ever becoming an argv entry that `ps` can see.
func runCreateAdmin(ctx context.Context) error {
	cfg, log, err := loadConfig()
	if err != nil {
		return err
	}

	args := os.Args[2:]
	if len(args) < 2 {
		return errors.New(
			"usage: control-api create-admin <email> <display name>\n" +
				"The password is read from the terminal, or from stdin when piped.\n" +
				"It is never taken as an argument: an argument is visible in ps and " +
				"in your shell history.")
	}
	email := strings.TrimSpace(args[0])
	name := strings.TrimSpace(strings.Join(args[1:], " "))
	if email == "" || name == "" {
		return errors.New("both an email address and a display name are required")
	}

	password, err := readPassword()
	if err != nil {
		return err
	}

	// The same policy the HTTP layer enforces. A bootstrap path that accepted a
	// weaker password than the application would be the weakest link, and it is
	// the one account that matters most.
	if err := auth.ValidatePassword(password, email, name); err != nil {
		return fmt.Errorf("password rejected: %w", err)
	}

	application, err := app.Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	// Refuse if any administrator already exists.
	//
	// This command is for bootstrapping an empty deployment. Leaving it able to
	// mint administrators on a running system would make it a privilege
	// escalation path for anyone who reaches the host, and administrators are
	// otherwise created through an audited HTTP route by an existing admin.
	existing, err := application.Store.Users.CountByRole(ctx, domain.RoleAdmin)
	if err != nil {
		return fmt.Errorf("could not check for existing administrators: %w", err)
	}
	if existing > 0 {
		return fmt.Errorf(
			"refusing: %d administrator account(s) already exist. This command only "+
				"bootstraps an empty deployment; create further administrators through "+
				"the admin interface, where the action is audited", existing)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	created, err := application.Store.Users.CreateUser(ctx, domain.User{
		Email: email, DisplayName: name, Role: domain.RoleAdmin, PasswordHash: hash,
	})
	if err != nil {
		return fmt.Errorf("create administrator: %w", err)
	}

	// The password is deliberately not echoed back. The operator supplied it
	// and knows it; repeating it here would write it to the scrollback and to
	// any log capturing this output.
	fmt.Printf("Administrator created.\n  email : %s\n  name  : %s\n  id    : %s\n",
		created.Email, created.DisplayName, created.ID)
	fmt.Println("\nSet up multi-factor authentication at first sign-in. This account can " +
		"administer users and verify the audit chain; it cannot trade.")
	return nil
}

// readPassword takes the password without putting it in argv or on screen.
func readPassword() (string, error) {
	fd := int(syscall.Stdin)
	if term.IsTerminal(fd) {
		fmt.Print("Password: ")
		first, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		fmt.Print("Confirm:  ")
		second, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if string(first) != string(second) {
			return "", errors.New("the two entries did not match")
		}
		return string(first), nil
	}

	// Piped. One line, no confirmation, because there is nobody to confirm with.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New(
			"no password on stdin. Pipe one, or run this from a terminal where it " +
				"can be typed without echo")
	}
	return strings.TrimRight(line, "\r\n"), nil
}
