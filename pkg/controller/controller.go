package controller

import (
	"context"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type MemgraphController struct {
	clientset kubernetes.Interface
	config    *Config
}

func NewMemgraphController(clientset kubernetes.Interface, config *Config) *MemgraphController {
	return &MemgraphController{
		clientset: clientset,
		config:    config,
	}
}

func (c *MemgraphController) TestConnection() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pods, err := c.clientset.CoreV1().Pods(c.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + c.config.AppName,
	})
	if err != nil {
		return err
	}

	log.Printf("Successfully connected to Kubernetes API. Found %d pods with app=%s in namespace %s",
		len(pods.Items), c.config.AppName, c.config.Namespace)

	return nil
}