// Command p8s-licensed is the P8S licence server: it issues and sells
// P8S OS subscription keys and answers the daily check from p8sd.
//
//	p8s-licensed keygen                     make the signing key; prints the public key for internal/license/pubkeys.txt
//	p8s-licensed pubkey                     print the public key again
//	p8s-licensed issue -tier T -server ID[,ID] [-customer NAME] [-email E] [-months 12] [-note TEXT]
//	p8s-licensed list
//	p8s-licensed show ID                    licence details and its key
//	p8s-licensed extend ID [-months 12]
//	p8s-licensed rebind ID -server ID[,ID]  move a licence to other servers (reinstall, new hardware)
//	p8s-licensed revoke ID [-note TEXT]     cancel (servers drop it at their next daily check)
//	p8s-licensed unrevoke ID
//	p8s-licensed invoices
//	p8s-licensed release add -dir ~/P8SOS -version 0.6.0 [-notes TEXT] [-stable]
//	                                        bundle + sign a source folder; published on testing (and stable)
//	p8s-licensed release promote -version 0.6.0   tested: put it on stable for subscribers
//	p8s-licensed release list
//	p8s-licensed serve                      run the shop and the check API
//
// Every command takes -config (default /etc/p8s-licensed.json).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"p8sos/internal/license"
	"p8sos/internal/licsrv"
	"p8sos/internal/mail"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	if err := run(cmd, args); err != nil {
		fmt.Fprintln(os.Stderr, "p8s-licensed:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: p8s-licensed keygen|pubkey|issue|list|show|extend|rebind|revoke|unrevoke|invoices|release|serve [-config FILE] ...")
	os.Exit(2)
}

// split pulls a leading positional argument (the licence ID) out of args.
func split(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func run(cmd string, args []string) error {
	id, args := split(args)
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/p8s-licensed.json", "config file")
	tier := fs.String("tier", "", "business, hosting, partner or internal")
	server := fs.String("server", "", "server ID(s), comma separated")
	customer := fs.String("customer", "", "customer name")
	email := fs.String("email", "", "customer email")
	months := fs.Int("months", 12, "months")
	note := fs.String("note", "", "note")
	asJSON := fs.Bool("json", false, "JSON output")
	dir := fs.String("dir", ".", "release: P8S OS source folder")
	version := fs.String("version", "", "release: version name, e.g. 0.6.0")
	notes := fs.String("notes", "", "release: what changed")
	stable := fs.Bool("stable", false, "release add: also publish on stable")
	_ = fs.Parse(args)
	if id == "" && fs.NArg() > 0 {
		id = fs.Arg(0)
	}
	cfg, err := licsrv.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}

	if cmd == "keygen" {
		pub, err := licsrv.GenerateKey(cfg.SigningKey)
		if err != nil {
			return err
		}
		fmt.Printf("signing key written to %s — keep it secret and back it up (anyone with it can make licences)\n\n", cfg.SigningKey)
		fmt.Printf("public key (add this line to internal/license/pubkeys.txt and rebuild P8S OS):\n%s\n", pub)
		return nil
	}
	key, err := licsrv.LoadKey(cfg.SigningKey)
	if err != nil {
		return err
	}
	st := &licsrv.Store{Dir: cfg.DataDir, Key: key}
	need := func() error {
		if id == "" {
			return errors.New("which licence? give its ID, e.g. " + cmd + " L7K2ABCDEF")
		}
		return nil
	}
	show := func(r licsrv.Record) error {
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(r)
		}
		l := r.License
		state := "active"
		if r.Revoked {
			state = "REVOKED " + r.Note
		} else if time.Now().After(l.End()) {
			state = "expired"
		}
		fmt.Printf("licence   %s\ncustomer  %s <%s>\ntier      %s\nservers   %s\nvalid     %s to %s (%s)\n\nkey:\n%s\n",
			l.ID, l.Customer, l.Email, l.Tier, strings.Join(l.Servers, ", "), l.Issued, l.Expires, state, r.Key)
		return nil
	}

	switch cmd {
	case "pubkey":
		fmt.Println(licsrv.PublicKey(key))
	case "issue":
		if _, ok := license.Tier(*tier); !ok || *tier == license.Home {
			return errors.New("-tier: business, hosting, partner or internal")
		}
		r, err := st.Issue(*tier, []string{*server}, *customer, *email, *months, *note)
		if err != nil {
			return err
		}
		return show(r)
	case "list":
		l, err := st.List()
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(l)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTIER\tCUSTOMER\tEXPIRES\tSERVERS\tSTATE")
		for _, r := range l {
			state := "active"
			if r.Revoked {
				state = "revoked"
			} else if time.Now().After(r.License.End()) {
				state = "expired"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", r.License.ID, r.License.Tier, r.License.Customer, r.License.Expires, len(r.License.Servers), state)
		}
		return tw.Flush()
	case "show":
		if err := need(); err != nil {
			return err
		}
		r, err := st.Get(id)
		if err != nil {
			return err
		}
		return show(r)
	case "extend":
		if err := need(); err != nil {
			return err
		}
		r, err := st.Extend(id, *months)
		if err != nil {
			return err
		}
		return show(r)
	case "rebind":
		if err := need(); err != nil {
			return err
		}
		r, err := st.Rebind(id, []string{*server})
		if err != nil {
			return err
		}
		return show(r)
	case "revoke", "unrevoke":
		if err := need(); err != nil {
			return err
		}
		r, err := st.Revoke(id, *note, cmd == "revoke")
		if err != nil {
			return err
		}
		fmt.Printf("%s: revoked=%v\n", r.License.ID, r.Revoked)
	case "invoices":
		l, err := st.Invoices()
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(l)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NUMBER\tDATE\tLICENCE\tBUYER\tTOTAL")
		for _, in := range l {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d.%02d %s\n", in.Number, in.Date, in.User, in.Buyer.Name, in.Total/100, in.Total%100, in.Currency)
		}
		return tw.Flush()
	case "release":
		switch id {
		case "add":
			r, err := st.AddRelease(*dir, *version, *notes, *stable)
			if err != nil {
				return err
			}
			fmt.Printf("published %s on %s (%d KB, sha256 %s)\n", r.Version, strings.Join(r.Channels, " + "), r.Size/1024, r.SHA256[:16])
		case "promote":
			r, err := st.Promote(*version)
			if err != nil {
				return err
			}
			fmt.Printf("%s is now on %s\n", r.Version, strings.Join(r.Channels, " + "))
		case "list", "":
			l, err := st.Releases()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tCHANNELS\tPUBLISHED\tSIZE\tNOTES")
			for _, r := range l {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d KB\t%s\n", r.Version, strings.Join(r.Channels, ","), r.Published[:10], r.Size/1024, r.Notes)
			}
			return tw.Flush()
		default:
			return errors.New("release add|promote|list")
		}
	case "serve":
		return serve(cfg, st)
	default:
		usage()
	}
	return nil
}

func serve(cfg *licsrv.Config, st *licsrv.Store) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	m := &mail.Mailer{Dir: cfg.DataDir, Config: func() mail.Config {
		return mail.Config{Host: cfg.SMTPHost, Port: cfg.SMTPPort, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.MailFrom}
	}}
	srv := &http.Server{Addr: cfg.Listen, Handler: (&licsrv.Server{Cfg: cfg, Store: st, Mail: m, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second, WriteTimeout: time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Info("p8s-licensed listening", "addr", cfg.Listen, "payments", cfg.RazorpayKeyID != "")
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
