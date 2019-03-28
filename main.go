package main

import (
	"flag"
	"fmt"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
	"github.com/mmcdole/gofeed"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/xanzy/go-gitlab"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

var addr = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
var lastRunGauge prometheus.Gauge
var issuesCreatedCounter prometheus.Counter

type Config struct {
	Feeds    []Feed
	Interval int
}

type Feed struct {
	ID              string
	FeedURL         string `yaml:"feed_url"`
	Name            string
	GitlabProjectID int `yaml:"gitlab_project_id"`
	Labels          []string
	AddedSince      time.Time `yaml:"added_since"`
}

type SyncedItems struct {
	gorm.Model
	UUID string
	Feed string
}

type EnvValues struct {
	DataDir      string
	ConfDir      string
	GitlabAPIKey string
}

func hasExistingGitlabIssue(guid string, projectID int, gitlabClient *gitlab.Client) bool {
	searchOptions := &gitlab.SearchOptions{
		Page:    1,
		PerPage: 10,
	}
	issues, _, err := gitlabClient.Search.IssuesByProject(projectID, guid, searchOptions)
	if err != nil {
		fmt.Printf("Unable to query Gitlab for existing issues\n")
	}
	retVal := false
	if len(issues) == 1 {
		retVal = true
		fmt.Printf("Found existing issues for %s in project (%s)\n", guid, issues[0].WebURL)

	} else if len(issues) > 1 {
		retVal = true
		var urls []string
		for _, issue := range issues {
			urls = append(urls, issue.WebURL)
		}
		fmt.Printf("Found multiple existing issues for %s in project (%s)\n", guid, strings.Join(urls, ", "))
	}

	return retVal

}

func (feed Feed) checkFeed(db *gorm.DB, gitlabClient *gitlab.Client) {
	fp := gofeed.NewParser()
	rss, err := fp.ParseURL(feed.FeedURL)

	if err != nil {
		fmt.Printf("Unable to parse feed %s: \n %s", feed.Name, err)
		return
	}

	var newArticle []*gofeed.Item
	var oldArticle []*gofeed.Item
	for _, item := range rss.Items {
		found := !db.First(&SyncedItems{}, "feed = ? AND uuid = ?", feed.ID, item.GUID).RecordNotFound()
		if found == true {
			oldArticle = append(oldArticle, item)
		} else {
			newArticle = append(newArticle, item)
		}
	}

	fmt.Printf("Feed Name: %s\n", feed.Name)
	fmt.Printf("Old Items: %d\n", len(oldArticle))
	fmt.Printf("New Items: %d\n", len(newArticle))

	for _, item := range newArticle {
		var time *time.Time
		// Prefer updated time to published
		if item.UpdatedParsed != nil {
			time = item.UpdatedParsed
		} else {
			time = item.PublishedParsed
		}

		if time.Before(feed.AddedSince) {
			fmt.Printf("Ignoring %s as its date is < the specified AddedSince (Item: %s vs AddedSince: %s)\n",
				item.Title, time, feed.AddedSince)
			continue
		}

		// Check Gitlab to see if we already have a matching issue there
		if hasExistingGitlabIssue(item.GUID, feed.GitlabProjectID, gitlabClient) {
			// We think its new but there is already a matching GUID in Gitlab.  Mark as Sync'd
			db.Create(&SyncedItems{UUID: item.GUID, Feed: feed.ID})
			continue
		}

		// Prefer description over content
		var body string
		if item.Description != "" {
			body = item.Description
		} else {
			body = item.Content
		}

		issueOptions := &gitlab.CreateIssueOptions{
			Title:       gitlab.String(item.Title),
			Description: gitlab.String(body + "\n" + item.GUID),
			Labels:      feed.Labels,
			CreatedAt:   time,
		}

		if _, _, err := gitlabClient.Issues.CreateIssue(feed.GitlabProjectID, issueOptions); err != nil {
			fmt.Printf("Unable to create Gitlab issue for %s \n %s \n", feed.Name, err)
			continue
		}
		if err := db.Create(&SyncedItems{UUID: item.GUID, Feed: feed.ID}).Error; err != nil {
			fmt.Printf("Unable to persist in %s DB: %s \n", item.Title, err)
			continue
		}
		issuesCreatedCounter.Inc()
		fmt.Printf("Created Gitlab Issue '%s' in project: %d' \n", item.Title, feed.GitlabProjectID)
	}
}

func readConfig(path string) *Config {
	config := &Config{}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatalln(err)
	}

	if err = yaml.Unmarshal(data, config); err != nil {
		fmt.Printf("Unable to parse config YAML \n %s \n", err)
		panic(err)
	}

	return config
}

func initialise(env EnvValues) (db *gorm.DB, client *gitlab.Client, config *Config) {
	gaugeOpts := prometheus.GaugeOpts{
		Name: "last_run_time",
		Help: "Last Run Time in Unix Seconds",
	}
	lastRunGauge = prometheus.NewGauge(gaugeOpts)
	prometheus.MustRegister(lastRunGauge)

	issuesCreatedCounterOpts := prometheus.CounterOpts{
		Name: "issues_created",
		Help: "Number of issues created in Gitlab",
	}
	issuesCreatedCounter = prometheus.NewCounter(issuesCreatedCounterOpts)
	prometheus.MustRegister(issuesCreatedCounter)

	client = gitlab.NewClient(nil, env.GitlabAPIKey)
	config = readConfig(path.Join(env.ConfDir, "config.yaml"))

	db, err := gorm.Open("sqlite3", path.Join(env.DataDir, "state.db"))
	if err != nil {
		panic(err)
	}

	db.AutoMigrate(&SyncedItems{})

	return
}

func main() {
	env := readEnv()
	db, gitlabClient, config := initialise(env)
	defer db.Close()

	go func() {
		for {
			fmt.Printf("Running checks at %s\n", time.Now().Format(time.RFC850))
			for _, configEntry := range config.Feeds {
				configEntry.checkFeed(db, gitlabClient)
			}
			lastRunGauge.SetToCurrentTime()
			time.Sleep(time.Duration(config.Interval) * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(*addr, nil))

}

func readEnv() EnvValues {
	var gitlabPAToken, configDir, dataDir string
	if envGitlabAPIToken := os.Getenv("GITLAB_API_TOKEN"); envGitlabAPIToken == "" {
		panic("Could not find GITLAB_API_TOKEN specified as an environment variable")
	} else {
		gitlabPAToken = envGitlabAPIToken
	}
	if envConfigDir := os.Getenv("CONFIG_DIR"); envConfigDir == "" {
		panic("Could not find CONFIG_DIR specified as an environment variable")
	} else {
		configDir = envConfigDir
	}
	if envDataDir := os.Getenv("DATA_DIR"); envDataDir == "" {
		panic("Could not find DATA_DIR specified as an environment variable")
	} else {
		dataDir = envDataDir
	}

	return EnvValues{
		DataDir:      dataDir,
		ConfDir:      configDir,
		GitlabAPIKey: gitlabPAToken,
	}
}
