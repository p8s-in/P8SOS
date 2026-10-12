package mail

import (
	"fmt"
	"strings"
)

// Brand is what every message is signed with.
type Brand struct {
	Name    string // "P8S Cloud"
	URL     string // panel address, e.g. https://panel.example.com
	Support string // support email
}

func (b Brand) footer() string {
	s := "\n— " + b.Name
	if b.URL != "" {
		s += "\n" + b.URL
	}
	if b.Support != "" {
		s += "\nHelp: " + b.Support
	}
	return s + "\n"
}

func (b Brand) link(path string) string {
	if b.URL == "" {
		return "(open the panel) " + path
	}
	return b.URL + path
}

// Message is a ready email.
type Message struct{ Subject, Body string }

func Verify(b Brand, user, code string) Message {
	return Message{"Confirm your email for " + b.Name, fmt.Sprintf(
		"Hello %s,\n\nplease confirm your email address to finish creating your %s account:\n\n%s\n\nThe link works for 48 hours. If you didn't sign up, ignore this email.\n%s",
		user, b.Name, b.link("/#verify="+code), b.footer())}
}

func Welcome(b Brand, user string) Message {
	return Message{"Welcome to " + b.Name, fmt.Sprintf(
		"Hello %s,\n\nyour account is ready. Add credit under Billing, then create your first server in a minute.\n\nSign in: %s\n\nTip: turn on two-factor sign-in under your name (top right).\n%s",
		user, b.link("/"), b.footer())}
}

func Reset(b Brand, user, code string) Message {
	return Message{"Reset your " + b.Name + " password", fmt.Sprintf(
		"Hello %s,\n\nsomeone (hopefully you) asked to reset your password. Choose a new one here:\n\n%s\n\nThe link works for 1 hour. If it wasn't you, ignore this email; your password stays as it is.\n%s",
		user, b.link("/#reset="+code), b.footer())}
}

// ServerReady tells the owner how to reach a new server.
func ServerReady(b Brand, user, vm, ip, login, plan string) Message {
	where := "Its address appears in the panel in about a minute."
	if ip != "" {
		where = "IP address: " + ip
	}
	how := "Log in with the SSH key or root password you chose:\n  ssh " + login + "@" + nz(ip, "<its IP>")
	return Message{fmt.Sprintf("Your server %s is ready", vm), fmt.Sprintf(
		"Hello %s,\n\nyour server %s (%s) is running.\n\n%s\n%s\n\nManage it, open its console or take backups in the panel: %s\n%s",
		user, vm, plan, where, how, b.link("/"), b.footer())}
}

func nz(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Money formats an amount in the smallest unit, e.g. 59000 INR -> ₹590.00.
func Money(amount int64, currency string) string {
	sign := ""
	if amount < 0 {
		sign, amount = "-", -amount
	}
	sym := map[string]string{"INR": "₹", "USD": "$", "EUR": "€", "GBP": "£"}[currency]
	if sym == "" {
		sym = currency + " "
	}
	return fmt.Sprintf("%s%s%d.%02d", sign, sym, amount/100, amount%100)
}

func PaymentReceived(b Brand, user, invoice string, credit, total int64, currency string) Message {
	return Message{"Payment received — invoice " + invoice, fmt.Sprintf(
		"Hello %s,\n\nthank you! We received %s; %s was added to your balance.\nInvoice %s is in the panel under Billing (print or save it as PDF).\n%s",
		user, Money(total, currency), Money(credit, currency), invoice, b.footer())}
}

func LowBalance(b Brand, user string, balance int64, currency string, hours int) Message {
	return Message{"Your balance is running low", fmt.Sprintf(
		"Hello %s,\n\nyour balance is %s, enough for about %s of your current servers.\nAdd credit to keep them running: %s\n%s",
		user, Money(balance, currency), dur(hours), b.link("/#billing"), b.footer())}
}

func Negative(b Brand, user string, balance int64, currency string, graceHours int) Message {
	return Message{"Action needed: your balance is negative", fmt.Sprintf(
		"Hello %s,\n\nyour balance is %s. Please add credit within %s, otherwise your servers will be stopped (their data is kept).\n\n%s\n%s",
		user, Money(balance, currency), dur(graceHours), b.link("/#billing"), b.footer())}
}

func Suspended(b Brand, user string, balance int64, currency string, vms []string) Message {
	return Message{"Your servers were stopped", fmt.Sprintf(
		"Hello %s,\n\nyour balance (%s) stayed negative, so we stopped your servers: %s.\nTheir disks and data are kept. Add credit and you can start them again right away:\n\n%s\n%s",
		user, Money(balance, currency), strings.Join(vms, ", "), b.link("/#billing"), b.footer())}
}

func Resumed(b Brand, user string) Message {
	return Message{"Thanks — your account is active again", fmt.Sprintf(
		"Hello %s,\n\nyour balance is positive again. You can start your servers in the panel: %s\n%s", user, b.link("/"), b.footer())}
}

func TicketReply(b Brand, user, subject, from, text string) Message {
	return Message{"Re: " + subject, fmt.Sprintf(
		"Hello %s,\n\n%s replied to your support request \"%s\":\n\n%s\n\nReply in the panel: %s\n%s", user, from, subject, indent(text), b.link("/#support"), b.footer())}
}

func TicketNew(b Brand, user, subject, text string) Message {
	return Message{"New support request: " + subject, fmt.Sprintf("%s wrote:\n\n%s\n\nAnswer in the panel: %s\n%s", user, indent(text), b.link("/#support"), b.footer())}
}

func indent(s string) string {
	if len(s) > 4000 {
		s = s[:4000] + "…"
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

func dur(hours int) string {
	switch {
	case hours < 1:
		return "less than an hour"
	case hours < 48:
		return fmt.Sprintf("%d hours", hours)
	default:
		return fmt.Sprintf("%d days", hours/24)
	}
}
