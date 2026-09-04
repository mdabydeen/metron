package main

import (
	"fmt"
	"io"
	"time"

	"github.com/mdabydeen/metron/internal/config"
	"github.com/mdabydeen/metron/internal/session"
)

// recorder keeps the on-disk transcript in step with the conversation.
//
// Saving happens when a turn finishes rather than when the operator remembers
// to ask, because the session people most want back is the one that ended in a
// crash or a closed terminal.
type recorder struct {
	store   session.Store
	meta    session.Meta
	enabled bool
	warn    io.Writer
	// warned stops a failing save from printing the same complaint on every
	// turn for the rest of the session.
	warned bool
}

func newRecorder(store session.Store, cfg config.Config, warn io.Writer) *recorder {
	head, dirty := session.Head(store.Root)
	return &recorder{
		store:   store,
		enabled: cfg.SaveSessions,
		warn:    warn,
		meta: session.Meta{
			ID:      session.NewID(time.Now()),
			Started: time.Now().UTC(),
			Model:   cfg.Model,
			GitHead: head,
			Dirty:   dirty,
		},
	}
}

// save writes the transcript. A failure is reported once and never fatal:
// losing the ability to record a conversation is not a reason to end it.
func (r *recorder) save(bot stepper) {
	if !r.enabled {
		return
	}
	if err := r.store.Save(r.meta, bot.Messages()); err != nil && !r.warned {
		r.warned = true
		fmt.Fprintf(r.warn, "\033[33mwarning: could not save session: %v\033[0m\n", err)
	}
}

// path is where this session is being written, for /sessions and the banner.
func (r *recorder) path() string { return r.store.Path(r.meta.ID) }

// resume loads a saved conversation into the agent and adopts its identity, so
// continuing a session appends to it rather than forking a second copy.
func (r *recorder) resume(id string, bot stepper, out io.Writer) error {
	meta, messages, err := r.store.Load(id)
	if err != nil {
		return err
	}
	bot.Restore(messages)
	r.meta = meta

	fmt.Fprintf(out, "Resumed session %s (%d messages).\n", meta.ID, len(messages))
	if drift := session.DriftWarning(meta, r.store.Root); drift != "" {
		// The transcript is full of line numbers and quoted code. If the tree
		// has moved underneath it the model will act confidently on things that
		// are no longer true, and only the operator can judge whether that is
		// acceptable.
		fmt.Fprintf(out, "\033[33mwarning: %s; the conversation may refer to code that has changed\033[0m\n", drift)
	}
	return nil
}

// listSessions prints the saved transcripts, marking the one in progress.
func listSessions(out io.Writer, sess *recorder) {
	ids, err := sess.store.List()
	if err != nil {
		fmt.Fprintf(out, "\033[31mError: %v\033[0m\n", err)
		return
	}
	if len(ids) == 0 {
		fmt.Fprintln(out, "No saved sessions.")
		return
	}
	for _, id := range ids {
		marker := " "
		if id == sess.meta.ID {
			marker = "*"
		}
		fmt.Fprintf(out, "%s %s\n", marker, id)
	}
	fmt.Fprintln(out, "\nResume one with: metron --resume <id>   (or --resume-last)")
}
