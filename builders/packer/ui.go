package packer

import (
	"fmt"
	"io"
	"strings"
)

// Ui forwards the plugin's messages to Print, one call per line, and its
// questions to Question. The Secrets values are masked in the messages.
type Ui struct {
	Print    func(line string)
	Question func(query string) (string, error)
	Secrets  []string
}

func (u *Ui) Say(msg string) {
	for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		u.Print(u.mask(line))
	}
}

func (u *Ui) Sayf(format string, a ...any) { u.Say(fmt.Sprintf(format, a...)) }

func (u *Ui) Message(msg string) { u.Say(msg) }

func (u *Ui) Error(msg string) { u.Say("Error: " + msg) }

func (u *Ui) Errorf(format string, a ...any) { u.Error(fmt.Sprintf(format, a...)) }

// Machine is the machine-readable output of Packer: not shown.
func (u *Ui) Machine(string, ...string) {}

func (u *Ui) Ask(query string) (string, error) { return u.Question(u.mask(query)) }

func (u *Ui) Askf(format string, a ...any) (string, error) { return u.Ask(fmt.Sprintf(format, a...)) }

// TrackProgress shows no progress bar.
func (u *Ui) TrackProgress(_ string, _, _ int64, stream io.ReadCloser) io.ReadCloser { return stream }

func (u *Ui) mask(s string) string {
	for _, secret := range u.Secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "<sensitive>")
		}
	}
	return s
}
