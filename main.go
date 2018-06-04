package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/lestrrat-go/slack"
	"github.com/lestrrat-go/slack/objects"
	redmine "github.com/mattn/go-redmine"
)

type options struct {
	Redmine redmineOptions
	Slack   slackOptions
}

type redmineOptions struct {
	APIKey   string `short:"k" long:"redmine-apikey" env:"REDMINE_APIKEY" required:"true" description:"APIKey for your Redmine"`
	Endpoint string `short:"r" long:"redmine-endpoint" env:"REDMINE_ENDPOINT" requireid:"true" description:"Endpoint URL of your Redmine"`
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

const (
	// maxLimit is maximum Limit for Redmine's issue API.
	maxLimit = 100
)

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
	log.Print("parse flags")
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
	log.Print("start to collect issues")
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
	log.Print("getIssues")
	cli := redmine.NewClient(opts.Endpoint, opts.APIKey)
	cli.Limit = maxLimit
	cli.Offset = 0 // initialize offset (default is -1)
	ris := []redmine.Issue{}

	// redmine's issues API returns 100 issues at a maximum.
	for {
		res, err := cli.Issues()
		if err != nil {
			return nil, err
		}
		if len(res) == 0 { // no more issues
			break
		}
		ris = append(ris, res...)
		cli.Offset += maxLimit
	}
	return convertIssues(ris), nil
}

func convertIssues(ris []redmine.Issue) []issue {
	log.Print("convertIssues")
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
	log.Print("finish collect issues")
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
		fmt.Fprintf(&buf, "- %s <%s/issues/%d|#%d>: %s(%s)\n", unassignable(formatTime(is.DueDate), "期日"), opts.Redmine.Endpoint, is.ID, is.ID, is.Subject, unassignable(getUser(opts, is.AssignedTo), "担当"))
	}
	fmt.Fprintf(&out, "期限切れのチケットは *%d件* です\n", ec)
	buf.WriteTo(&out)
	buf.Reset()
	var nc int
	for is := range nearCh {
		nc++
		fmt.Fprintf(&buf, "- %s <%s/issues/%d|#%d>: %s(%s)\n", unassignable(formatTime(is.DueDate), "期日"), opts.Redmine.Endpoint, is.ID, is.ID, is.Subject, unassignable(getUser(opts, is.AssignedTo), "担当"))
	}
	fmt.Fprintf(&out, "期限切れが近いチケットは *%d件* です\n", nc)
	buf.WriteTo(&out)
	log.Print("post to slack")
	if _, err := cli.Chat().PostMessage(opts.Slack.Channel).LinkNames(true).Text(out.String()).Do(context.Background()); err != nil {
		return err
	}
	return nil
}

func unassignable(target, label string) string {
	if target == "" {
		return fmt.Sprintf("%s未設定", label)
	}
	return target
}

func getUser(opts options, idname *redmine.IdName) string {
	if idname == nil {
		return ""
	}
	redmineClient := redmine.NewClient(opts.Redmine.Endpoint, opts.Redmine.APIKey)
	redmineUser, err := redmineClient.User(idname.Id)
	if err != nil {
		return idname.Name
	}
	slackRESTClient := slack.New(opts.Slack.Token)
	slackUserList, err := slackRESTClient.Users().List().Do(context.Background())
	if err != nil {
		return idname.Name
	}
	for _, slackUser := range slackUserList {
		if isSameUser(*redmineUser, *slackUser) {
			return "<@" + slackUser.ID + ">"
		}
	}

	return idname.Name
}

func isSameUser(redmineUser redmine.User, slackUser objects.User) bool {
	realName := strings.Replace(slackUser.RealName, "　", " ", -1)
	if redmineUser.Login == slackUser.Name {
		return true
	}
	switch realName {
	case
		redmineUser.Lastname + redmineUser.Firstname,
		redmineUser.Lastname + " " + redmineUser.Firstname,
		redmineUser.Firstname + redmineUser.Lastname,
		redmineUser.Firstname + " " + redmineUser.Lastname:

		return true
	}
	if mappedName, ok := userMap[slackUser.RealName]; ok {
		slackUser.RealName = mappedName
		return isSameUser(redmineUser, slackUser)
	}
	return false
}

func formatTime(t time.Time) string {
	s := t.Format("2006-01-02")
	if s == "0001-01-01" {
		return ""
	}
	return s
}
