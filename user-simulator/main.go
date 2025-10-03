package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
)

type UserActivity struct {
	UserID    string `json:"user_id"`
	Timestamp int64  `json:"timestamp"`
}

type UserConnect struct {
	UserID string `json:"user_id"`
}

type UserDisconnect struct {
	UserID string `json:"user_id"`
}

type UserType int

const (
	FastUser   UserType = iota // Connects and disconnects quickly (3 users)
	SlowUser                   // Connects and stays longer (17 users)
	BrowseUser                 // Only browses, never connects (30 users)
)

type User struct {
	ID          string
	Type        UserType
	IsActive    bool
	IsConnected bool
	LastActivity time.Time
	SessionStart time.Time
}

type UserSimulator struct {
	redisClient *redis.Client
	ctx         context.Context
	users       []*User
}

func NewUserSimulator() *UserSimulator {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	users := make([]*User, 50)
	
	// Create 3 fast users (connect/disconnect quickly)
	for i := 0; i < 3; i++ {
		users[i] = &User{
			ID:   uuid.New().String(),
			Type: FastUser,
		}
	}
	
	// Create 17 slow users (stay connected longer)
	for i := 3; i < 20; i++ {
		users[i] = &User{
			ID:   uuid.New().String(),
			Type: SlowUser,
		}
	}
	
	// Create 30 browse-only users
	for i := 20; i < 50; i++ {
		users[i] = &User{
			ID:   uuid.New().String(),
			Type: BrowseUser,
		}
	}

	return &UserSimulator{
		redisClient: rdb,
		ctx:         context.Background(),
		users:       users,
	}
}

func (us *UserSimulator) Start() {
	log.Println("Starting user simulator...")

	go us.simulateUserActivity()
	go us.simulateUserSessions()

	select {}
}

func (us *UserSimulator) simulateUserActivity() {
	ticker := time.NewTicker(time.Duration(rand.Intn(1000)+300) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Select a random user for activity
			user := us.users[rand.Intn(len(us.users))]
			
			// All users can browse/have activity
			user.LastActivity = time.Now()
			user.IsActive = true
			
			activity := UserActivity{
				UserID:    user.ID,
				Timestamp: time.Now().Unix(),
			}

			data, _ := json.Marshal(activity)
			err := us.redisClient.Publish(us.ctx, "user:activity", data).Err()
			if err != nil {
				log.Printf("Failed to publish user activity: %v", err)
			} else {
				log.Printf("Published user activity: %s (type: %v)", user.ID[:8], user.Type)
			}

			ticker.Reset(time.Duration(rand.Intn(1000)+300) * time.Millisecond)
		}
	}
}

func (us *UserSimulator) simulateUserSessions() {
	// Start individual session simulators for each connecting user
	for _, user := range us.users {
		if user.Type == FastUser || user.Type == SlowUser {
			go us.simulateUserSession(user)
		}
	}
	
	// Keep the function running
	select {}
}

func (us *UserSimulator) simulateUserSession(user *User) {
	for {
		// Wait for user to become active before attempting to connect
		us.waitForUserActivity(user)
		
		if user.IsConnected {
			continue
		}
		
		// Wait some time after activity before connecting
		var waitTime time.Duration
		if user.Type == FastUser {
			waitTime = time.Duration(rand.Intn(30)+5) * time.Second
		} else {
			waitTime = time.Duration(rand.Intn(60)+15) * time.Second
		}
		
		time.Sleep(waitTime)
		
		// Connect the user
		us.connectUser(user)
		
		// Simulate session duration
		var sessionDuration time.Duration
		if user.Type == FastUser {
			// Fast users: 1-3 minutes
			sessionDuration = time.Duration(rand.Intn(120)+60) * time.Second
		} else {
			// Slow users: 3-10 minutes
			sessionDuration = time.Duration(rand.Intn(420)+180) * time.Second
		}
		
		time.Sleep(sessionDuration)
		
		// Disconnect the user
		us.disconnectUser(user)
		
		// Wait before next session
		nextSessionWait := time.Duration(rand.Intn(180)+60) * time.Second
		time.Sleep(nextSessionWait)
	}
}

func (us *UserSimulator) waitForUserActivity(user *User) {
	for {
		if user.IsActive && time.Since(user.LastActivity) < 30*time.Second {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

func (us *UserSimulator) connectUser(user *User) {
	user.IsConnected = true
	user.SessionStart = time.Now()
	
	connect := UserConnect{
		UserID: user.ID,
	}

	data, _ := json.Marshal(connect)
	err := us.redisClient.Publish(us.ctx, "user:connect", data).Err()
	if err != nil {
		log.Printf("Failed to publish user connect: %v", err)
	} else {
		userType := "SLOW"
		if user.Type == FastUser {
			userType = "FAST"
		}
		log.Printf("🔗 User connected: %s (%s)", user.ID[:8], userType)
	}
}

func (us *UserSimulator) disconnectUser(user *User) {
	if !user.IsConnected {
		return
	}
	
	sessionDuration := time.Since(user.SessionStart)
	user.IsConnected = false
	
	// Track total connection time in Redis
	err := us.redisClient.IncrByFloat(us.ctx, fmt.Sprintf("user:total_time:%s", user.ID), sessionDuration.Seconds()).Err()
	if err != nil {
		log.Printf("Failed to update user total time in Redis: %v", err)
	}
	
	disconnect := UserDisconnect{
		UserID: user.ID,
	}

	data, _ := json.Marshal(disconnect)
	err = us.redisClient.Publish(us.ctx, "user:disconnect", data).Err()
	if err != nil {
		log.Printf("Failed to publish user disconnect: %v", err)
	} else {
		userType := "SLOW"
		if user.Type == FastUser {
			userType = "FAST"
		}
		log.Printf("❌ User disconnected: %s (%s) - session: %.1fm", 
			user.ID[:8], userType, sessionDuration.Minutes())
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())

	simulator := NewUserSimulator()

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Println("User simulator started. Press Ctrl+C to stop.")
	simulator.Start()
}
