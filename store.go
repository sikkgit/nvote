package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/rdbell/nvote/schemas"

	checkErr "github.com/rdbell/nvote/check"

	"github.com/fiatjaf/go-nostr"
	"github.com/labstack/echo/v4"
	_ "github.com/mattn/go-sqlite3"
)

// db is a sqlite DB for storying/querying posts
var db *sql.DB

// initSQLite initializes the sqlite conn to an in-memory DB
func initSQLite() {
	var err error
	db, err = sql.Open("sqlite3", "file::memory:?mode=memory&cache=shared")
	checkErr.Panic(err)
	db.SetMaxOpenConns(100)

	// Set busy timeout
	_, err = db.Exec(`PRAGMA busy_timeout = 5000`)
	checkErr.Panic(err)

	// Disable journaling
	_, err = db.Exec(`PRAGMA journal_mode = OFF`)
	checkErr.Panic(err)

	// Exclusive locking mode
	_, err = db.Exec(`PRAGMA locking_mode = EXCLUSIVE`)
	checkErr.Panic(err)
}

// setupPostsTables initializes the posts table in SQLite
func setupPostsTable() {
	_, err := db.Exec(`
	create table posts (id TEXT NOT NULL PRIMARY KEY, score INTEGER, user_score INT, ranking FLOAT, children INTEGER, pubkey TEXT, created_at INTEGER, title TEXT, body TEXT, channel TEXT, parent TEXT);
	create INDEX posts_id ON posts(id);
	create INDEX posts_ranking ON posts(ranking);
	create INDEX posts_pubkey ON posts(pubkey);
	create INDEX posts_channel ON posts(channel);
	create INDEX posts_parent ON posts(parent);
	delete from posts;
	`)
	checkErr.Panic(err)
}

// setupUsersTable initializes the users table in SQLite
func setupUsersTable() {
	_, err := db.Exec(`
	create table users (pubkey TEXT NOT NULL PRIMARY KEY, user_score INT);
	create INDEX users_pubkey ON posts(pubkey);
	delete from users;
	`)
	checkErr.Panic(err)
}

// setupVotesTable initializes the votes table in SQLite
func setupVotesTable() {
	_, err := db.Exec(`
	create table votes (pubkey TEXT, target TEXT, direction BOOLEAN, created_at INTEGER);
	create INDEX votes_pubkey ON votes(pubkey);
	create INDEX votes_target ON votes(target);
	delete from votes;
	`)
	checkErr.Panic(err)
}

// subEvents sets up the nostr relay pool and subscribes to events
func fetchEvents() {
	pool = nostr.NewRelayPool()
	checkErr.Panic(pool.Add(appConfig.Relay, &nostr.SimplePolicy{Read: true, Write: true}))

	go func() {
		for notice := range pool.Notices {
			log.Printf("%s has sent a notice: '%s'\n", notice.Relay, notice.Message)
		}
	}()

	// Get nostr events
	sub := pool.Sub(nostr.EventFilters{
		{
			Kinds: []int{nostr.KindTextNote},
		},
	})

	go func() {
		for event := range sub.UniqueEvents {
			// Validate event signature
			if ok, _ := event.CheckSignature(); !ok {
				continue
			}

			// TODO: also return status code to prevent trying insertPost if it was a vote but didn't succeed?
			if insertVote(&event) == nil {
				continue
			}

			fmt.Println(insertPost(&event))
		}
	}()
}

// publishEvent submits a user's event to the nostr network
func publishEvent(c echo.Context, content []byte) (*nostr.Event, error) {
	// Create a new nostr event
	event := &nostr.Event{
		CreatedAt: uint32(time.Now().Unix()),
		Kind:      nostr.KindTextNote,
		Tags:      make(nostr.Tags, 0),
		Content:   string(content),
	}

	// Validate public/private keys
	pub, err := nostr.GetPublicKey(c.Get("user").(*schemas.User).PrivKey)
	if err != nil || pub != c.Get("user").(*schemas.User).PubKey {
		clearCookie(c, "user")
		return event, errors.New("invalid keypair")
	}

	// Sign event
	event.PubKey = pub
	err = event.Sign(c.Get("user").(*schemas.User).PrivKey)

	if err != nil {
		clearCookie(c, "user")
		return event, err
	}

	// Publish event
	result, publishStatus, err := pool.PublishEvent(event)
	if err != nil {
		return result, err
	}

	for {
		select {
		case status, ok := <-publishStatus:
			if !ok {
				return event, errors.New("unable to publish")
			}

			// Sent
			if status.Status == 0 {
				fmt.Println("Event sent.")
				time.Sleep(1 * time.Second)
			}

			// Send failed
			if status.Status == -1 {
				return result, errors.New("failed to publish event")
			}

			// Sent successfully
			if status.Status == 1 {
				// Redirect to post
				// TODO: use another event channel that waits for an event that fires after the post is inserted into the sqlite db
				// to prevent redirecting too early
				return result, nil
			}
			fmt.Println(status.Status)
		case <-time.After(time.Second * 10):
			return event, errors.New("timeout during publish")
		}
	}
}