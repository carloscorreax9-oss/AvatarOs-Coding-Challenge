package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

type NodeStatus struct {
	NodeID string `json:"node_id"`
	Status string `json:"status"`
}

type CreateNodeResponse struct {
	ID string `json:"id"`
}

type NodeManager struct {
	redisClient *redis.Client
	ctx         context.Context
	nodes       map[string]string
	nodeStartTimes map[string]time.Time
	mutex       sync.RWMutex
}

func NewNodeManager() *NodeManager {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	return &NodeManager{
		redisClient: rdb,
		ctx:         context.Background(),
		nodes:       make(map[string]string),
		nodeStartTimes: make(map[string]time.Time),
	}
}

func (nm *NodeManager) CreateNode(w http.ResponseWriter, r *http.Request) {
	nodeID := fmt.Sprintf("node-%s", uuid.New().String()[:8])

	nm.mutex.Lock()
	nm.nodes[nodeID] = "booting"
	nm.nodeStartTimes[nodeID] = time.Now()
	nm.mutex.Unlock()

	statusMsg := NodeStatus{
		NodeID: nodeID,
		Status: "booting",
	}

	data, _ := json.Marshal(statusMsg)
	err := nm.redisClient.Publish(nm.ctx, "node:status", data).Err()
	if err != nil {
		log.Printf("Failed to publish node status: %v", err)
	}

	go nm.simulateNodeBooting(nodeID)

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(CreateNodeResponse{ID: nodeID})

	log.Printf("Created node: %s", nodeID)
}

func (nm *NodeManager) DeleteNode(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	nodeID := vars["node_id"]

	nm.mutex.Lock()
	if _, exists := nm.nodes[nodeID]; !exists {
		nm.mutex.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Track total running time in Redis before terminating
	if startTime, exists := nm.nodeStartTimes[nodeID]; exists {
		runningDuration := time.Since(startTime)
		err := nm.redisClient.IncrByFloat(nm.ctx, fmt.Sprintf("node:total_time:%s", nodeID), runningDuration.Seconds()).Err()
		if err != nil {
			log.Printf("Failed to update node total time in Redis: %v", err)
		}
		delete(nm.nodeStartTimes, nodeID)
	}

	nm.nodes[nodeID] = "terminated"
	nm.mutex.Unlock()

	statusMsg := NodeStatus{
		NodeID: nodeID,
		Status: "terminated",
	}

	data, _ := json.Marshal(statusMsg)
	err := nm.redisClient.Publish(nm.ctx, "node:status", data).Err()
	if err != nil {
		log.Printf("Failed to publish node status: %v", err)
	}

	w.WriteHeader(http.StatusAccepted)

	log.Printf("Terminated node: %s", nodeID)
}

func (nm *NodeManager) simulateNodeBooting(nodeID string) {
	bootTime := time.Duration(10+rand.Intn(20)) * time.Second
	time.Sleep(bootTime)

	nm.mutex.Lock()
	if status, exists := nm.nodes[nodeID]; exists && status != "terminated" {
		nm.nodes[nodeID] = "ready"
		nm.mutex.Unlock()

		statusMsg := NodeStatus{
			NodeID: nodeID,
			Status: "ready",
		}

		data, _ := json.Marshal(statusMsg)
		err := nm.redisClient.Publish(nm.ctx, "node:status", data).Err()
		if err != nil {
			log.Printf("Failed to publish node status: %v", err)
		} else {
			log.Printf("Node %s is now ready", nodeID)
		}
	} else {
		nm.mutex.Unlock()
	}
}

func (nm *NodeManager) HealthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "node-api",
	})
}

func (nm *NodeManager) GetEfficiency(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	
	// Get all user connection times
	userKeys, err := nm.redisClient.Keys(nm.ctx, "user:total_time:*").Result()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to get user times"})
		return
	}
	
	var totalUserTime float64
	for _, key := range userKeys {
		timeStr, err := nm.redisClient.Get(nm.ctx, key).Result()
		if err != nil {
			continue
		}
		if userTime, err := strconv.ParseFloat(timeStr, 64); err == nil {
			totalUserTime += userTime
		}
	}
	
	// Get all node running times
	nodeKeys, err := nm.redisClient.Keys(nm.ctx, "node:total_time:*").Result()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to get node times"})
		return
	}
	
	var totalNodeTime float64
	for _, key := range nodeKeys {
		timeStr, err := nm.redisClient.Get(nm.ctx, key).Result()
		if err != nil {
			continue
		}
		if nodeTime, err := strconv.ParseFloat(timeStr, 64); err == nil {
			totalNodeTime += nodeTime
		}
	}
	
	// Calculate efficiency (user time / node time)
	efficiency := 0.0
	if totalNodeTime > 0 {
		efficiency = (totalUserTime / totalNodeTime) * 100
	}
	
	response := map[string]interface{}{
		"total_user_time_seconds": totalUserTime,
		"total_node_time_seconds": totalNodeTime,
		"efficiency_percentage": efficiency,
		"user_count": len(userKeys),
		"node_count": len(nodeKeys),
	}
	
	json.NewEncoder(w).Encode(response)
}

func main() {
	nodeManager := NewNodeManager()

	r := mux.NewRouter()
	r.HandleFunc("/", nodeManager.HealthCheck).Methods("GET")
	r.HandleFunc("/api/nodes", nodeManager.CreateNode).Methods("POST")
	r.HandleFunc("/api/nodes/{node_id}", nodeManager.DeleteNode).Methods("DELETE")
	r.HandleFunc("/api/efficiency", nodeManager.GetEfficiency).Methods("GET")

	port := os.Getenv("PORT")
	if port == "" {
		port = "7777"
	}

	log.Printf("Node management API starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}
