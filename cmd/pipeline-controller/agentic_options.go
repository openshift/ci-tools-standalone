package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/prow/pkg/flagutil"
)

const defaultAgenticTimeout = 20 * time.Minute

// Chai trust and response deadlines are deployment-wide.
type agenticOptions struct {
	timeout        time.Duration
	trustedAuthors flagutil.Strings
}

func (o *agenticOptions) addFlags(fs *flag.FlagSet) {
	fs.DurationVar(&o.timeout, "agentic-timeout", defaultAgenticTimeout, "Time to wait for any Chai comment after publishing readiness before traditional dispatch.")
	fs.Var(&o.trustedAuthors, "agentic-trusted-author", "Exact GitHub login whose comments cancel the agentic response timeout and may mark its gate. Repeat for multiple authors; no default.")
}

func (o *agenticOptions) validate() error {
	if o.timeout <= 0 {
		return fmt.Errorf("--agentic-timeout must be a positive duration")
	}
	for _, author := range o.trustedAuthors.Strings() {
		if author == "" || strings.ContainsAny(author, " \t\r\n/@,") {
			return fmt.Errorf("invalid --agentic-trusted-author %q: use an exact GitHub login", author)
		}
	}
	return nil
}

func (o *agenticOptions) validateEnabled() error {
	if err := o.validate(); err != nil {
		return err
	}
	if len(o.trustedAuthors.Strings()) == 0 {
		return fmt.Errorf("agentic mode requires --agentic-trusted-author")
	}
	return nil
}

func (a *agenticController) validateEnrollment() error {
	if a.hasAgenticEnrollment() {
		if a.appID <= 0 {
			return fmt.Errorf("agentic mode requires GitHub App authentication")
		}
		return a.options.validateEnabled()
	}
	return nil
}
