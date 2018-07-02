package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
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

type redmineUserMap struct {
	m sync.Map
}

func (rum *redmineUserMap) Set(id int, user redmine.User) {
	rum.m.Store(id, user)
}

func (rum *redmineUserMap) Get(id int) (redmine.User, error) {
	ui, ok := rum.m.Load(id)
	if !ok {
		return redmine.User{}, errors.New("the user is not found")
	}
	u, ok := ui.(redmine.User)
	if !ok {
		return redmine.User{}, errors.New("cannot convert to *redmine.User")
	}
	return u, nil
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

var (
	userMap       = loadUserMap()
	slackClient   *slack.Client
	slackUsers    objects.UserList
	redmineClient *redmine.Client
	redmineUsers  redmineUserMap
)

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
	initialize(opts)
	iss, err := getIssues(opts.Redmine)
	if err != nil {
		return err
	}
	out := fanout(iss, isExpired, isNear)

	return postToSlack(opts, out[0], out[1])
}

func initialize(opts options) {
	log.Print("initialize clients")
	slackClient = slack.New(opts.Slack.Token)
	redmineClient = redmine.NewClient(opts.Redmine.Endpoint, opts.Redmine.APIKey)
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

func loadRedmineUsers() error {
	users, err := redmineClient.Users()
	if err != nil {
		return err
	}
	for _, user := range users {
		redmineUsers.Set(user.Id, user)
	}
	return nil
}

func loadSlackUsers() error {
	users, err := slackClient.Users().List().Do(context.Background())
	if err != nil {
		return err
	}
	slackUsers = users
	return nil
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
		log.Printf("got %d - %d: %d", cli.Offset, cli.Offset+cli.Limit, len(res))
		if len(res) == 0 { // no more issues
			break
		}
		ris = append(ris, res...)
		cli.Offset += maxLimit
		if len(res) > cli.Limit {
			break
		}
	}
	log.Printf("issues: %d", len(ris))
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

func isExpired(is issue) bool {
	return today. /*Is*/ After(is.DueDate)
}

func isNear(is issue) bool {
	return !isExpired(is) && weekend. /*Is*/ After(is.DueDate)
}

func postToSlack(opts options, expiredCh, nearCh <-chan issue) error {
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
	redmineUser, err := redmineUsers.Get(idname.Id)
	if err != nil {
		return idname.Name
	}
	for _, slackUser := range slackUsers {
		if isSameUser(redmineUser, *slackUser) {
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

func fanout(in []issue, filters ...func(issue) bool) []chan issue {
	n := len(filters)
	out := make([]chan issue, n)
	for i := 0; i < n; i++ {
		out[i] = make(chan issue, 10)
	}

	go func(in []issue, out []chan issue) {
		defer func(out []chan issue) {
			log.Print("fanout finished")
			for _, c := range out {
				close(c)
			}
		}(out)

		for _, iss := range in {
			for i := range out {
				if filters[i](iss) {
					out[i] <- iss
				}
			}
		}
	}(in, out)

	return out
}
