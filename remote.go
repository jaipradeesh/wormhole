package wormhole

import (
	"io/ioutil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garyburd/redigo/redis"
	handler "github.com/superfly/wormhole/remote"
	"github.com/superfly/wormhole/session"
)

var (
	listenPort    = os.Getenv("PORT")
	nodeID        = os.Getenv("NODE_ID")
	redisURL      = os.Getenv("REDIS_URL")
	localhost     = os.Getenv("LOCALHOST")
	privateKey    = os.Getenv("PRIVATE_KEY")
	clusterURL    = os.Getenv("CLUSTER_URL")
	sessions      map[string]session.Session
	redisPool     *redis.Pool
	sshPrivateKey []byte
)

// StartRemote ...
func StartRemote(cfg *Config) {
	ensureRemoteEnvironment()
	go handleDeath()

	var h handler.Handler
	var err error

	switch cfg.Protocol {
	case SSH:
		h, err = handler.NewSSHHandler(sshPrivateKey, localhost, clusterURL, nodeID, redisPool, sessions)
		if err != nil {
			log.Fatal(err)
		}
	case TCP:
		h, err = handler.NewTCPHandler(localhost, clusterURL, nodeID, redisPool, sessions)
		if err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal("Unknown wormhole transport layer protocol selected.")
	}

	handler.ListenAndServe(":"+listenPort, h)
}

func ensureRemoteEnvironment() {
	ensureEnvironment()
	var err error
	sshPrivateKeyFile := os.Getenv("SSH_PRIVATE_KEY")
	sshPrivateKey, err = ioutil.ReadFile(sshPrivateKeyFile)
	if err != nil {
		log.Fatalf("Failed to load private key (%s)", sshPrivateKeyFile)
	}

	if clusterURL == "" {
		log.Fatalln("CLUSTER_URL is required.")
	}
	if listenPort == "" {
		listenPort = "10000"
	}
	if redisURL == "" {
		log.Fatalln("REDIS_URL is required.")
	}

	redisPool = newRedisPool(redisURL)

	redisConn := redisPool.Get()
	defer redisConn.Close()
	_, err = redisConn.Do("PING")
	if err != nil {
		log.Fatalf("Couldn't connect to Redis: %s", err.Error())
	}

	if nodeID == "" {
		nodeID, _ = os.Hostname()
	}

	sessions = make(map[string]session.Session)
}

func newRedisPool(redisURL string) *redis.Pool {
	return &redis.Pool{
		MaxIdle:     3,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			conn, err := redis.DialURL(redisURL)
			if err != nil {
				return nil, err
			}

			parsedURL, err := url.Parse(redisURL)
			if err != nil {
				return nil, err
			}
			if parsedURL.User != nil {
				if password, hasPassword := parsedURL.User.Password(); hasPassword == true {
					if _, authErr := conn.Do("AUTH", password); authErr != nil {
						conn.Close()
						return nil, authErr
					}
				}
			}
			return conn, nil
		},
		TestOnBorrow: func(conn redis.Conn, t time.Time) error {
			if time.Since(t) < time.Minute {
				return nil
			}
			_, err := conn.Do("PING")
			return err
		},
	}
}

// IT CAN BE HANDLED!
func handleDeath() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func(c <-chan os.Signal) {
		for _ = range c {
			log.Print("Cleaning up before exit...")
			for id, session := range sessions {
				session.Close()
				delete(sessions, id)
			}
			log.Print("Cleaned up connections.")
			os.Exit(1)
		}
	}(c)
}
