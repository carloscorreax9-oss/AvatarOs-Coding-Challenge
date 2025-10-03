# AvatarOS Backend Engineering Code Challenge (Go)

#### **1. Objective**

Build a **Predictive Node Provisioning Service** in Go. Your service must ensure a GPU node is ready and waiting the instant a user requests one, by leveraging signals from user activity before they connect. The goal is to minimize user wait time while controlling costs by minimizing idle nodes.

#### **2. Provided Infrastructure**

To simulate the environment, we provide the following:

*   **Redis Pub/Channels:** Your service must subscribe to these channels:
    *   `user:activity`: Published by our main API whenever a user makes a request (e.g., browsing avatars). Message format: `{"user_id": "uuid", "timestamp": 1234567890}`
    *   `user:connect`: Published when a user attempts to start a session. Message format: `{"user_id": "uuid"}`
    *   `user:disconnect`: Published when a user ends their session. Message format: `{"user_id": "uuid"}`
    *   `node:status`: Published by the node manager on state changes. Message format: `{"node_id": "node-123", "status": "booting|ready|terminated"}`

*   **Node Management API (Mock):** A REST API you will call to manage nodes.
    *   `POST /api/nodes`: Creates a new node. Returns `202` with `{"id": "node-123"}`
    *   `DELETE /api/nodes/{node_id}`: Terminates a node. Returns `202`.
    *   *(Note: There's a simulated delay between creation and a `node:ready` status update).*

*   **User Simulator (Mock):** A separate service will be running that generates fake traffic on the `user:activity` and `user:connect` channels.

#### **3. Your Task**

Write a Go service that:

1.  **Listens** to the Redis Pub/Sub channels `user:activity`, `user:connect`, and `node:status`.
2.  **Maintains an internal state** (using Redis or in-memory data structures with persistence to Redis) to track:
    *   Which users are "active" (recently seen on `user:activity`).
    *   The current state of all nodes (`booting`, `ready`, `allocated`).
    *   Which node is allocated to which user.
3.  **Implements a Predictive Algorithm:** Uses the stream of `user:activity` events to proactively spin up nodes *before* `user:connect` events happen. You must design this algorithm yourself. A simple heuristic is acceptable, but you should be able to explain its reasoning and trade-offs.
4.  **Handles `user:connect` events:** When a user wants to connect, they must be immediately assigned a `ready` node. If no node is available, this is considered a failure (and the user would experience latency in a real system).
5.  **Manages the Node Pool:** Scales the pool of nodes up based on predictions and scales it down when nodes are idle for too long to save costs.

#### **4. Requirements & Constraints**

*   Language: **Go**
*   You must use the provided Redis channels and Node Management API.
*   Your solution should be efficient, concurrent, and production-grade (handle errors, log appropriately, avoid race conditions).
*   Include a `docker-compose.yml` file to set up any dependencies (Redis) for easy testing.

#### **5. Deliverables**

1.  **Source Code:** A well-structured Go module.
2.  **README.md:** Documentation containing:
    *   How to build and run your service.
    *   A detailed explanation of your **design choices**, especially your **predictive algorithm** and **data structures**.
    *   An analysis of the trade-offs your system makes (e.g., how it balances cost vs. latency).
    *   A paragraph on "What I would improve with more time."
3.  **Dockerfile** and **docker-compose.yml** (Highly desirable).

#### **6. Evaluation Criteria**

We will assess your submission on:

*   **Functionality:** Does it work correctly? Does it minimize "connect misses"?
*   **Algorithm Design:** The quality and justification of your predictive scaling heuristic.
*   **Code Quality:** Clarity, organization, error handling, logging, and adherence to Go best practices.
*   **Concurrency:** Correct use of goroutines, channels, and mutexes to handle real-time events.
*   **Data Modeling:** Smart use of Redis data structures to maintain state efficiently and reliably.

#### **7. Follow-Up Interview**

Be prepared to discuss:
*   Your algorithm's reasoning and how you would improve it.
*   How your system would handle a sudden traffic spike.
*   How you would add a feature to prioritize certain users over others.
*   How you would debug a scenario where nodes are spinning up but users are still waiting.

