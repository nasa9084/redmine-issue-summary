package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strconv"
	"time"

	flags "github.com/jessevdk/go-flags"
	redmine "github.com/mattn/go-redmine"
)

type options struct {
	RedmineEndpoint string `short:"r" long:"redmine" env:"REDMINE_URL" required:"true" description:"URL of your redmine"`
	RedmineAPIKey   string `short:"k" long:"apikey" env:"REDMINE_APIKEY" required:"true" description:"apikey to use redmine API"`
	ProjectID       string `short:"p" long:"project" env:"REDMINE_PROJECT" required:"true" description:"project id to summary"`
}

const timeFmt = "2006-01-02"

var (
	now         = time.Now()
	weekend     = now.Add(time.Duration(time.Friday-now.Weekday()) * 24 * time.Hour)
	usermapping = loadUserMapping()
)

type issue struct {
	ID           string
	Subject      string
	DueDate      string
	AssignedUser string
	Ref          string
}

func convert(cli *redmine.Client, rissue redmine.Issue, endpoint string) issue {
	return issue{
		ID:           strconv.Itoa(rissue.Id),
		Subject:      rissue.Subject,
		DueDate:      rissue.DueDate,
		AssignedUser: rmID2slID(cli, rissue.AssignedTo),
		Ref:          path.Join(endpoint, "issues", strconv.Itoa(rissue.Id)),
	}
}

func (i issue) String() string {
	return fmt.Sprintf("%s <%s|%s>: %s(@%s)", i.DueDate, i.Ref, i.ID, i.Subject, i.AssignedUser)
}

func rmID2slID(cli *redmine.Client, idname *redmine.IdName) string {
	if uid, ok := usermapping[strconv.Itoa(idname.Id)]; ok {
		return uid
	}
	u, err := cli.User(idname.Id)
	if err != nil {
		if uid, ok := usermapping[idname.Name]; ok {
			return uid
		}
		return idname.Name
	}
	if uid, ok := usermapping[u.Login]; ok {
		return uid
	}
	return u.Login
}

func loadUserMapping() map[string]string {
	f, err := os.Open("usermapping.json")
	if err != nil {
		return map[string]string{}
	}
	defer f.Close()

	m := map[string]string{}
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return map[string]string{}
	}
	return m
}

func main() {
	if err := exec(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
	os.Exit(0)
}

func exec() error {
	var opts options
	if _, err := flags.Parse(&opts); err != nil {
		return err
	}

	cli := redmine.NewClient(opts.RedmineEndpoint, opts.RedmineAPIKey)
	f := redmine.IssueFilter{ProjectId: opts.ProjectID}

	issues, err := cli.IssuesByFilter(&f)
	if err != nil {
		return err
	}

	expiredChan := make(chan issue, 1)
	go collect(expiredChan, cli, opts.RedmineEndpoint, issues, isExpired)
	nearChan := make(chan issue, 1)
	go collect(nearChan, cli, opts.RedmineEndpoint, issues, isNear)

	fprintIssues(os.Stdout, "expired", expiredChan)
	fprintIssues(os.Stdout, "near", nearChan)
	return nil
}

func collect(ch chan issue, cli *redmine.Client, endpoint string, issues []redmine.Issue, filterFn func(redmine.Issue) bool) {
	for _, i := range issues {
		if filterFn(i) {
			ch <- convert(cli, i, endpoint)
		}
	}
	close(ch)
}

func isExpired(i redmine.Issue) bool {
	due, err := time.ParseInLocation(timeFmt, i.DueDate, time.Local)
	if err != nil {
		return false
	}
	return now.After(due)
}

func isNear(i redmine.Issue) bool {
	due, err := time.ParseInLocation(timeFmt, i.DueDate, time.Local)
	if err != nil {
		return false
	}
	d := due.Sub(weekend) / 24
	days := (d.Minutes() - d.Hours()) / 60
	return days < 7
}

func fprintIssues(out io.Writer, label string, issCh chan issue) {
	c := 0
	var buf bytes.Buffer
	for i := range issCh {
		fmt.Fprintln(&buf, i.String())
		c++
	}
	fmt.Fprintf(out, "%s: %d\n", label, c)
	fmt.Fprint(out, buf.String())
}
