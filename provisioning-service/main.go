package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

// Message types for Redis channels
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

type NodeStatus struct {
	NodeID string `json:"node_id"`
	Status string `json:"status"`
}

// Internal data structures
type User struct {
	ID              string
	LastActivity    time.Time
	IsActive        bool
	IsConnected     bool
	ActivityScore   float64
	PredictionScore float64
}

type Node struct {
	ID           string
	Status       string // "booting", "ready", "allocated"
	AllocatedTo  string
	CreatedAt    time.Time
	LastUsed     time.Time
	IdleDuration time.Duration
}

type PredictiveProvisioningService struct {
	redisClient    *redis.Client
	httpClient     *http.Client
	ctx            context.Context
	nodeAPIURL     string
	
	// State management
	users          map[string]*User
	nodes          map[string]*Node
	readyNodes     []string // Queue of ready nodes
	userMutex      sync.RWMutex
	nodeMutex      sync.RWMutex
	
	// Configuration
	activityWindow    time.Duration // How long to consider user activity
	predictionWindow  time.Duration // How far ahead to predict
	minReadyNodes     int           // Minimum number of ready nodes to maintain
	maxIdleTime       time.Duration // Max time a node can be idle before termination
	scaleUpThreshold  float64       // Threshold for scaling up
	scaleDownThreshold float64      // Threshold for scaling down
	
	// Metrics
	totalConnections    int
	missedConnections   int
	totalNodesCreated   int
	totalNodesTerminated int
}

func NewPredictiveProvisioningService() *PredictiveProvisioningService {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	
	nodeAPIURL := os.Getenv("NODE_API_URL")
	if nodeAPIURL == "" {
		nodeAPIURL = "http://localhost:8080"
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	return &PredictiveProvisioningService{
		redisClient:         rdb,
		httpClient:          &http.Client{Timeout: 30 * time.Second},
		ctx:                 context.Background(),
		nodeAPIURL:          nodeAPIURL,
		users:               make(map[string]*User),
		nodes:               make(map[string]*Node),
		readyNodes:          make([]string, 0),
		activityWindow:      5 * time.Minute,
		predictionWindow:    2 * time.Minute,
		minReadyNodes:       2,
		maxIdleTime:         10 * time.Minute,
		scaleUpThreshold:    0.7,
		scaleDownThreshold:  0.3,
	}
}

func (pps *PredictiveProvisioningService) Start() {
	log.Println("🚀 Starting Predictive Node Provisioning Service...")
	
	// Start Redis subscription
	go pps.subscribeToRedisChannels()
	
	// Start prediction engine
	go pps.predictionEngine()
	
	// Start node management
	go pps.nodeManagement()
	
	// Start metrics reporting
	go pps.metricsReporter()
	
	// Keep the service running
	select {}
}

func (pps *PredictiveProvisioningService) subscribeToRedisChannels() {
	pubsub := pps.redisClient.Subscribe(pps.ctx, "user:activity", "user:connect", "user:disconnect", "node:status")
	defer pubsub.Close()

	ch := pubsub.Channel()
	
	for msg := range ch {
		switch msg.Channel {
		case "user:activity":
			pps.handleUserActivity(msg.Payload)
		case "user:connect":
			pps.handleUserConnect(msg.Payload)
		case "user:disconnect":
			pps.handleUserDisconnect(msg.Payload)
		case "node:status":
			pps.handleNodeStatus(msg.Payload)
		}
	}
}

func (pps *PredictiveProvisioningService) handleUserActivity(payload string) {
	var activity UserActivity
	if err := json.Unmarshal([]byte(payload), &activity); err != nil {
		log.Printf("Error unmarshaling user activity: %v", err)
		return
	}

	pps.userMutex.Lock()
	defer pps.userMutex.Unlock()

	now := time.Now()
	user, exists := pps.users[activity.UserID]
	if !exists {
		user = &User{
			ID:           activity.UserID,
			LastActivity: now,
			IsActive:     true,
			IsConnected:  false,
		}
		pps.users[activity.UserID] = user
	} else {
		user.LastActivity = now
		user.IsActive = true
	}

	// Update activity score based on recency and frequency
	pps.updateUserActivityScore(user)
	
	log.Printf("📊 User activity: %s (score: %.2f)", activity.UserID[:8], user.ActivityScore)
}

func (pps *PredictiveProvisioningService) handleUserConnect(payload string) {
	var connect UserConnect
	if err := json.Unmarshal([]byte(payload), &connect); err != nil {
		log.Printf("Error unmarshaling user connect: %v", err)
		return
	}

	pps.userMutex.Lock()
	user, exists := pps.users[connect.UserID]
	if exists {
		user.IsConnected = true
	}
	pps.userMutex.Unlock()

	pps.nodeMutex.Lock()
	defer pps.nodeMutex.Unlock()

	// Try to allocate a ready node
	if len(pps.readyNodes) > 0 {
		nodeID := pps.readyNodes[0]
		pps.readyNodes = pps.readyNodes[1:]
		
		if node, exists := pps.nodes[nodeID]; exists {
			node.Status = "allocated"
			node.AllocatedTo = connect.UserID
			node.LastUsed = time.Now()
			pps.totalConnections++
			
			log.Printf("✅ User %s connected to node %s", connect.UserID[:8], nodeID)
		}
	} else {
		pps.missedConnections++
		log.Printf("❌ No ready nodes available for user %s (missed connection)", connect.UserID[:8])
	}
}

func (pps *PredictiveProvisioningService) handleUserDisconnect(payload string) {
	var disconnect UserDisconnect
	if err := json.Unmarshal([]byte(payload), &disconnect); err != nil {
		log.Printf("Error unmarshaling user disconnect: %v", err)
		return
	}

	pps.userMutex.Lock()
	if user, exists := pps.users[disconnect.UserID]; exists {
		user.IsConnected = false
	}
	pps.userMutex.Unlock()

	pps.nodeMutex.Lock()
	defer pps.nodeMutex.Unlock()

	// Find and free the node allocated to this user
	for _, node := range pps.nodes {
		if node.AllocatedTo == disconnect.UserID && node.Status == "allocated" {
			node.Status = "ready"
			node.AllocatedTo = ""
			node.LastUsed = time.Now()
			pps.readyNodes = append(pps.readyNodes, node.ID)
			
			log.Printf("🔄 User %s disconnected, node %s is now ready", disconnect.UserID[:8], node.ID)
			break
		}
	}
}

func (pps *PredictiveProvisioningService) handleNodeStatus(payload string) {
	var status NodeStatus
	if err := json.Unmarshal([]byte(payload), &status); err != nil {
		log.Printf("Error unmarshaling node status: %v", err)
		return
	}

	pps.nodeMutex.Lock()
	defer pps.nodeMutex.Unlock()

	node, exists := pps.nodes[status.NodeID]
	if !exists {
		node = &Node{
			ID:        status.NodeID,
			CreatedAt: time.Now(),
		}
		pps.nodes[status.NodeID] = node
	}

	oldStatus := node.Status
	node.Status = status.Status

	switch status.Status {
	case "ready":
		if oldStatus != "ready" {
			pps.readyNodes = append(pps.readyNodes, status.NodeID)
			log.Printf("🟢 Node %s is ready", status.NodeID)
		}
	case "terminated":
		// Remove from ready nodes if it was there
		for i, readyNodeID := range pps.readyNodes {
			if readyNodeID == status.NodeID {
				pps.readyNodes = append(pps.readyNodes[:i], pps.readyNodes[i+1:]...)
				break
			}
		}
		delete(pps.nodes, status.NodeID)
		pps.totalNodesTerminated++
		log.Printf("🔴 Node %s terminated", status.NodeID)
	}
}

func (pps *PredictiveProvisioningService) updateUserActivityScore(user *User) {
	now := time.Now()
	
	// Calculate recency score (higher for more recent activity)
	recencyScore := math.Exp(-time.Since(user.LastActivity).Seconds() / 300) // 5-minute decay
	
	// Calculate frequency score based on activity pattern
	frequencyScore := 1.0
	if user.IsConnected {
		frequencyScore = 1.5 // Connected users get higher priority
	}
	
	// Combine scores
	user.ActivityScore = recencyScore * frequencyScore
	
	// Calculate prediction score (likelihood to connect soon)
	user.PredictionScore = pps.calculatePredictionScore(user)
}

func (pps *PredictiveProvisioningService) calculatePredictionScore(user *User) float64 {
	now := time.Now()
	
	// Base prediction on activity recency and pattern
	timeSinceActivity := time.Since(user.LastActivity)
	
	// If user just became active, high prediction score
	if timeSinceActivity < 30*time.Second {
		return 0.9
	}
	
	// If user has been active recently, medium prediction score
	if timeSinceActivity < 2*time.Minute {
		return 0.6
	}
	
	// If user has been active in the last 5 minutes, low prediction score
	if timeSinceActivity < 5*time.Minute {
		return 0.3
	}
	
	return 0.0
}

func (pps *PredictiveProvisioningService) predictionEngine() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pps.makePredictions()
		}
	}
}

func (pps *PredictiveProvisioningService) makePredictions() {
	pps.userMutex.RLock()
	pps.nodeMutex.RLock()
	
	// Calculate current prediction score
	totalPredictionScore := 0.0
	activeUsers := 0
	
	for _, user := range pps.users {
		if user.IsActive && !user.IsConnected {
			pps.updateUserActivityScore(user)
			totalPredictionScore += user.PredictionScore
			activeUsers++
		}
	}
	
	readyNodesCount := len(pps.readyNodes)
	pps.userMutex.RUnlock()
	pps.nodeMutex.RUnlock()
	
	// Decision logic
	shouldScaleUp := false
	shouldScaleDown := false
	
	// Scale up if prediction score is high and we don't have enough ready nodes
	if totalPredictionScore > pps.scaleUpThreshold && readyNodesCount < pps.minReadyNodes {
		shouldScaleUp = true
	}
	
	// Scale up if we have many active users but few ready nodes
	if activeUsers > readyNodesCount*2 && readyNodesCount < 5 {
		shouldScaleUp = true
	}
	
	// Scale down if we have too many idle nodes
	if readyNodesCount > pps.minReadyNodes*2 && totalPredictionScore < pps.scaleDownThreshold {
		shouldScaleDown = true
	}
	
	if shouldScaleUp {
		pps.scaleUp()
	} else if shouldScaleDown {
		pps.scaleDown()
	}
	
	log.Printf("🎯 Prediction: score=%.2f, active=%d, ready=%d, scale_up=%v, scale_down=%v", 
		totalPredictionScore, activeUsers, readyNodesCount, shouldScaleUp, shouldScaleDown)
}

func (pps *PredictiveProvisioningService) scaleUp() {
	log.Println("📈 Scaling up: Creating new node...")
	
	// Create a new node via the Node Management API
	resp, err := pps.httpClient.Post(pps.nodeAPIURL+"/api/nodes", "application/json", nil)
	if err != nil {
		log.Printf("Error creating node: %v", err)
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusAccepted {
		log.Printf("Failed to create node: status %d", resp.StatusCode)
		return
	}
	
	var createResp struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		log.Printf("Error decoding create node response: %v", err)
		return
	}
	
	pps.totalNodesCreated++
	log.Printf("✅ Node creation initiated: %s", createResp.ID)
}

func (pps *PredictiveProvisioningService) scaleDown() {
	pps.nodeMutex.Lock()
	defer pps.nodeMutex.Unlock()
	
	// Find the most idle ready node
	var oldestNodeID string
	var oldestTime time.Time
	
	for _, nodeID := range pps.readyNodes {
		if node, exists := pps.nodes[nodeID]; exists {
			if oldestNodeID == "" || node.LastUsed.Before(oldestTime) {
				oldestNodeID = nodeID
				oldestTime = node.LastUsed
			}
		}
	}
	
	if oldestNodeID != "" && time.Since(oldestTime) > pps.maxIdleTime {
		log.Printf("📉 Scaling down: Terminating idle node %s", oldestNodeID)
		
		// Remove from ready nodes first
		for i, readyNodeID := range pps.readyNodes {
			if readyNodeID == oldestNodeID {
				pps.readyNodes = append(pps.readyNodes[:i], pps.readyNodes[i+1:]...)
				break
			}
		}
		
		// Terminate the node
		go pps.terminateNode(oldestNodeID)
	}
}

func (pps *PredictiveProvisioningService) terminateNode(nodeID string) {
	req, err := http.NewRequest("DELETE", pps.nodeAPIURL+"/api/nodes/"+nodeID, nil)
	if err != nil {
		log.Printf("Error creating terminate request: %v", err)
		return
	}
	
	resp, err := pps.httpClient.Do(req)
	if err != nil {
		log.Printf("Error terminating node %s: %v", nodeID, err)
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusAccepted {
		log.Printf("Failed to terminate node %s: status %d", nodeID, resp.StatusCode)
		return
	}
	
	log.Printf("✅ Node %s termination initiated", nodeID)
}

func (pps *PredictiveProvisioningService) nodeManagement() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pps.cleanupInactiveUsers()
			pps.updateNodeIdleTimes()
		}
	}
}

func (pps *PredictiveProvisioningService) cleanupInactiveUsers() {
	pps.userMutex.Lock()
	defer pps.userMutex.Unlock()
	
	now := time.Now()
	for userID, user := range pps.users {
		if !user.IsConnected && now.Sub(user.LastActivity) > pps.activityWindow {
			user.IsActive = false
		}
	}
}

func (pps *PredictiveProvisioningService) updateNodeIdleTimes() {
	pps.nodeMutex.Lock()
	defer pps.nodeMutex.Unlock()
	
	now := time.Now()
	for _, node := range pps.nodes {
		if node.Status == "ready" {
			node.IdleDuration = now.Sub(node.LastUsed)
		}
	}
}

func (pps *PredictiveProvisioningService) metricsReporter() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pps.reportMetrics()
		}
	}
}

func (pps *PredictiveProvisioningService) reportMetrics() {
	pps.userMutex.RLock()
	pps.nodeMutex.RLock()
	
	activeUsers := 0
	connectedUsers := 0
	readyNodes := len(pps.readyNodes)
	totalNodes := len(pps.nodes)
	
	for _, user := range pps.users {
		if user.IsActive {
			activeUsers++
		}
		if user.IsConnected {
			connectedUsers++
		}
	}
	
	successRate := 0.0
	if pps.totalConnections > 0 {
		successRate = float64(pps.totalConnections-pps.missedConnections) / float64(pps.totalConnections) * 100
	}
	
	pps.userMutex.RUnlock()
	pps.nodeMutex.RUnlock()
	
	log.Printf("📊 METRICS - Active: %d, Connected: %d, Ready Nodes: %d, Total Nodes: %d, Success Rate: %.1f%%, Created: %d, Terminated: %d",
		activeUsers, connectedUsers, readyNodes, totalNodes, successRate, pps.totalNodesCreated, pps.totalNodesTerminated)
}

func main() {
	service := NewPredictiveProvisioningService()
	service.Start()
}
