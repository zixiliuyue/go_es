// Package pool 提供Elasticsearch客户端连接池管理
// 支持多客户端连接管理、负载均衡、健康检查自动下线不健康节点
package pool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zixiliuyue/go_es/pkg/client"
	"github.com/zixiliuyue/go_es/pkg/errors"
	"go.uber.org/zap"
)

// NodeConfig 节点配置
type NodeConfig struct {
	Addresses []string `json:"addresses"`
	Username  string   `json:"username"`
	Password  string   `json:"password"`
	// Weight 节点权重，用于加权负载均衡，越高越优先
	Weight int `json:"weight"`
}

// PoolConfig 连接池配置
type PoolConfig struct {
	// Nodes 节点配置列表
	Nodes []NodeConfig
	// HealthCheckInterval 健康检查间隔
	HealthCheckInterval time.Duration
	// LoadBalancer 负载均衡算法：round_robin（轮询）或 weighted_round_robin（加权轮询）
	LoadBalancer string
	// Logger 日志记录器
	Logger *zap.Logger
}

// Pool 客户端连接池
type Pool struct {
	nodes    []*poolNode
	mu       sync.RWMutex
	logger   *zap.Logger
	config   PoolConfig
	stopChan chan struct{}
}

type poolNode struct {
	client     *client.Client
	config     NodeConfig
	healthy    bool
	lastCheck  time.Time
	weight     int // 静态权重
	currentWeight int // 当前权重（用于加权轮询）
}

// NewPool 创建一个新的连接池
func NewPool(config PoolConfig) (*Pool, error) {
	if config.HealthCheckInterval <= 0 {
		config.HealthCheckInterval = 30 * time.Second // 默认30秒检查一次
	}

	pool := &Pool{
		logger:   config.Logger,
		config:   config,
		stopChan: make(chan struct{}),
	}

	// 初始化所有节点
	for _, nodeConfig := range config.Nodes {
		// 创建客户端
		cfg := client.Config{
			Addresses: nodeConfig.Addresses,
			Username:  nodeConfig.Username,
			Password:  nodeConfig.Password,
			Logger:    config.Logger,
		}

		// 默认权重为1
		weight := nodeConfig.Weight
		if weight <= 0 {
			weight = 1
		}

		c, err := client.NewClient(cfg)
		if err != nil {
			if pool.logger != nil {
				pool.logger.Warn("Failed to create client for node",
					zap.String("addresses", fmt.Sprintf("%v", nodeConfig.Addresses)),
					zap.Error(err))
			}
			pool.nodes = append(pool.nodes, &poolNode{
				client:         nil,
				config:         nodeConfig,
				healthy:        false,
				weight:         weight,
				currentWeight:  weight,
			})
		} else {
			// 检查健康
			healthy, _ := c.Ping()
			pool.nodes = append(pool.nodes, &poolNode{
				client:         c,
				config:         nodeConfig,
				healthy:        healthy,
				weight:         weight,
				currentWeight:  weight,
			})
		}
	}

	// 启动后台健康检查
	go pool.startHealthChecker()

	return pool, nil
}

// Get 获取一个健康的客户端
// 根据配置使用不同的负载均衡算法
func (p *Pool) Get() (*client.Client, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	healthyNodes := p.getHealthyNodesLocked()
	if len(healthyNodes) == 0 {
		return nil, errors.New(errors.ErrCodeClientInit, "no healthy nodes available in pool")
	}

	// 检查负载均衡算法配置，默认使用加权轮询
	if p.config.LoadBalancer == "weighted_round_robin" || p.config.LoadBalancer == "" {
		return p.selectWeightedRoundRobin(healthyNodes), nil
	}

	// 默认简单轮询
	node := healthyNodes[0]
	// 轮询：把第一个移到最后，下一次取第二个
	// 这样循环利用
	p.moveFirstToEndLocked()

	return node.client, nil
}

// selectWeightedRoundRobin 使用加权轮询算法选择节点
// 按照权重概率选择，权重越高被选中的概率越大
func (p *Pool) selectWeightedRoundRobin(healthyNodes []*poolNode) *client.Client {
	// 平滑加权轮询算法
	// 参见: https://en.wikipedia.org/wiki/Weighted_round_robin#SWRR

	var (
		totalWeight int
		maxNode     *poolNode
		maxWeight   int
	)

	// 找到当前权重最大的节点，并累加所有节点权重
	for _, node := range healthyNodes {
		if node == nil || !node.healthy || node.client == nil {
			continue
		}

		totalWeight += node.weight

		// 增加当前权重
		node.currentWeight += node.weight

		if maxNode == nil || node.currentWeight > maxWeight {
			maxWeight = node.currentWeight
			maxNode = node
		}
	}

	if maxNode == nil {
		// 如果找不到，返回第一个健康节点
		return healthyNodes[0].client
	}

	// 减去总权重
	maxNode.currentWeight -= totalWeight

	return maxNode.client
}

// GetAllHealthy 获取所有健康节点客户端
func (p *Pool) GetAllHealthy() []*client.Client {
	p.mu.RLock()
	defer p.mu.RUnlock()

	nodes := p.getHealthyNodesLocked()
	clients := make([]*client.Client, 0, len(nodes))
	for _, node := range nodes {
		clients = append(clients, node.client)
	}
	return clients
}

// Stats 获取池统计信息
func (p *Pool) Stats() (total int, healthy int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	total = len(p.nodes)
	healthy = len(p.getHealthyNodesLocked())
	return
}

// Close 关闭连接池，释放所有连接
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	close(p.stopChan)

	for _, node := range p.nodes {
		if node.client != nil {
			node.client.Close()
		}
	}

	p.nodes = nil
}

func (p *Pool) getHealthyNodesLocked() []*poolNode {
	var healthy []*poolNode
	for _, node := range p.nodes {
		if node.healthy && node.client != nil {
			healthy = append(healthy, node)
		}
	}
	return healthy
}

func (p *Pool) moveFirstToEndLocked() {
	if len(p.nodes) <= 1 {
		return
	}
	// 找到第一个健康节点，移到列表末尾
	for i := 0; i < len(p.nodes); i++ {
		if p.nodes[i].healthy {
			// 找到第一个，移到末尾
			node := p.nodes[i]
			p.nodes = append(p.nodes[:i], p.nodes[i+1:]...)
			p.nodes = append(p.nodes, node)
			break
		}
	}
}

func (p *Pool) startHealthChecker() {
	ticker := time.NewTicker(p.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.checkHealth()
		case <-p.stopChan:
			return
		}
	}
}

func (p *Pool) checkHealth() {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, node := range p.nodes {
		if node.client == nil {
			// 尝试重新创建
			cfg := client.Config{
				Addresses: node.config.Addresses,
				Username:  node.config.Username,
				Password:  node.config.Password,
				Logger:    p.logger,
			}

			c, err := client.NewClient(cfg)
			if err != nil {
				node.healthy = false
				node.client = nil
				if p.logger != nil {
					p.logger.Warn("Failed to recreate client for node",
						zap.String("addresses", fmt.Sprintf("%v", node.config.Addresses)),
						zap.Error(err))
				}
				continue
			}
			node.client = c
		}

		// 检查ping
		healthy, err := node.client.Ping()
		oldHealthy := node.healthy
		node.healthy = healthy && err == nil
		node.lastCheck = time.Now()

		if oldHealthy != node.healthy {
			if node.healthy {
				p.logger.Info("Node became healthy",
					zap.String("addresses", fmt.Sprintf("%v", node.config.Addresses)))
			} else {
				p.logger.Warn("Node became unhealthy",
					zap.String("addresses", fmt.Sprintf("%v", node.config.Addresses)),
					zap.Error(err))
			}
		}
	}
}

// AddNode 添加一个新节点到连接池
func (p *Pool) AddNode(config NodeConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cfg := client.Config{
		Addresses: config.Addresses,
		Username:  config.Username,
		Password:  config.Password,
		Logger:    p.logger,
	}

	// 默认权重为1
	weight := config.Weight
	if weight <= 0 {
		weight = 1
	}

	c, err := client.NewClient(cfg)
	if err != nil {
		p.nodes = append(p.nodes, &poolNode{
			client:         nil,
			config:         config,
			healthy:        false,
			weight:         weight,
			currentWeight:  weight,
		})
		return err
	}

	healthy, _ := c.Ping()
	p.nodes = append(p.nodes, &poolNode{
		client:         c,
		config:         config,
		healthy:        healthy,
		weight:         weight,
		currentWeight:  weight,
	})

	return nil
}

// RemoveNode 从连接池移除一个节点
func (p *Pool) RemoveNode(address string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 查找并移除
	newNodes := make([]*poolNode, 0, len(p.nodes))
	removed := false
	for _, node := range p.nodes {
		// 简单匹配：地址包含该address
		found := false
		for _, addr := range node.config.Addresses {
			if addr == address {
				found = true
				break
			}
		}
		if !found {
			newNodes = append(newNodes, node)
			continue
		}

		if node.client != nil {
			node.client.Close()
		}
		removed = true
	}

	if !removed {
		return errors.New(errors.ErrCodeUnknown, fmt.Sprintf("node with address %s not found", address))
	}

	p.nodes = newNodes
	return nil
}

// HealthyCount 返回健康节点数量
func (p *Pool) HealthyCount() int {
	_, healthy := p.Stats()
	return healthy
}

// TotalCount 返回总节点数量
func (p *Pool) TotalCount() int {
	total, _ := p.Stats()
	return total
}
