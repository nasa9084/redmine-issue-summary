package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/lestrrat-go/slack"
	redmine "github.com/mattn/go-redmine"
)

type options struct {
	Redmine redmineOptions
	Slack   slackOptions
}

type redmineOptions struct {
	APIKey    string `short:"k" long:"redmine-apikey" env:"REDMINE_APIKEY" required:"true" description:"APIKey for your Redmine"`
	ProjectID string `short:"p" long:"redmine-projectid" env:"REDMINE_PROJECTID" required:"true" description:"Project ID you want to summarize"`
	Endpoint  string `short:"r" long:"redmine-endpoint" env:"REDMINE_ENDPOINT" requireid:"true" description:"Endpoint URL of your Redmine"`
}

type slackOptions struct {
	Token   string `short:"t" long:"slack-token" env:"SLACK_TOKEN" required:"true" description:"Slack API Token"`
	Channel string `short:"c" long:"slack-channel" env:"SLACK_CHANNEL" default:"#general" description:"Slack channel you want to post"`
}

type issue struct {
	ID         int
	Subject    string
	DueDate    time.Time
	AssignedTo *redmine.IdName
}

var (
	now     = time.Now()
	today   = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	weekend = today.Add(time.Duration(5-today.Weekday()) * time.Hour * 24)
)

var userMap = loadUserMap()

func main() { os.Exit(_main()) }

func _main() int {
	if err := exec(); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}

func exec() error {
	var opts options
	if _, err := flags.Parse(&opts); err != nil {
		if fe, ok := err.(*flags.Error); ok && fe.Type == flags.ErrHelp {
			return nil
		}
		return err
	}

	iss, err := getIssues(opts.Redmine)
	if err != nil {
		return err
	}
	expired := make(chan issue, 1)
	near := make(chan issue, 1)
	go collect(expired, iss, isExpired)
	go collect(near, iss, isNear)

	return postToSlack(opts, expired, near)
}

func loadUserMap() map[string]string {
	f, err := os.Open("./usermapping.json")
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

func getIssues(opts redmineOptions) ([]issue, error) {
	cli := redmine.NewClient(opts.Endpoint, opts.APIKey)
	ris, err := cli.IssuesByFilter(&redmine.IssueFilter{ProjectId: opts.ProjectID})
	if err != nil {
		return nil, err
	}
	return convertIssues(ris), nil
}

func convertIssues(ris []redmine.Issue) []issue {
	var is []issue
	for _, ri := range ris {
		due, _ := time.Parse("2006-01-02", ri.DueDate)
		is = append(is, issue{
			ID:         ri.Id,
			Subject:    ri.Subject,
			DueDate:    due,
			AssignedTo: ri.AssignedTo,
		})
	}
	return is
}

func collect(ch chan issue, iss []issue, filter func(issue) bool) {
	for _, is := range iss {
		if filter(is) {
			ch <- is
		}
	}
	close(ch)
}

func isExpired(is issue) bool {
	return today. /*Is*/ After(is.DueDate)
}

func isNear(is issue) bool {
	return !isExpired(is) && weekend. /*Is*/ After(is.DueDate)
}

func postToSlack(opts options, expiredCh, nearCh chan issue) error {
	cli := slack.New(opts.Slack.Token)
	if _, err := cli.Auth().Test().Do(context.Background()); err != nil {
		return err
	}
	var out bytes.Buffer
	var buf bytes.Buffer
	var ec int
	for is := range expiredCh {
		ec++
		fmt.Fprintf(&buf, "- %s <%s|#%d>: %s(<%s>)\n", is.DueDate.Format("2006-01-02"), opts.Redmine.Endpoint, is.ID, is.Subject, getUser(opts, is.AssignedTo))
	}
	fmt.Fprintf(&out, "期限切れのチケットは*%d件*です\n", ec)
	buf.WriteTo(&out)
	buf.Reset()
	var nc int
	for is := range nearCh {
		nc++
		fmt.Fprintf(&buf, "- %s <%s|#%d>: %s(<%s>)\n", is.DueDate.Format("2006-01-02"), opts.Redmine.Endpoint, is.ID, is.Subject, getUser(opts, is.AssignedTo))
	}
	fmt.Fprintf(&out, "期限切れが近いチケットは*%d件*です\n", nc)
	buf.WriteTo(&out)
	if _, err := cli.Chat().PostMessage(opts.Slack.Channel).LinkNames(true).Text(out.String()).Do(context.Background()); err != nil {
		return err
	}
	return nil
}

func getUser(opts options, idname *redmine.IdName) string {
	r := redmine.NewClient(opts.Redmine.Endpoint, opts.Redmine.APIKey)
	id := idname.Name
	if i, ok := userMap[idname.Name]; ok {
		id = i
	}
	ru, err := r.User(idname.Id)
	if err != nil {
		if id == "channel" {
			return "!" + id
		}
		return id
	}
	if login, ok := userMap[ru.Login]; ok {
		ru.Login = login
	}
	s := slack.New(opts.Slack.Token)
	sul, err := s.Users().List().Do(context.Background())
	if err != nil {
		return ru.Login
	}
	for _, su := range sul {
		if su.Name == ru.Login {
			return "@" + su.ID
		}
	}
	return ru.Login
}
