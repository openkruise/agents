package webhook

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gitlab.alibaba-inc.com/serverlessinfra/agents/utils/webhookutils"
	"gitlab.alibaba-inc.com/serverlessinfra/agents/utils/webhookutils/configuration"
	"gitlab.alibaba-inc.com/serverlessinfra/agents/utils/webhookutils/generator"
	"gitlab.alibaba-inc.com/serverlessinfra/agents/utils/webhookutils/writer"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	admissionregistrationinformers "k8s.io/client-go/informers/admissionregistration/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	validatingWebhookConfigurationName = "kruise-sandbox-validating-webhook-configuration"

	defaultResyncPeriod = time.Minute
)

var (
	namespace  = webhookutils.GetNamespace()
	secretName = webhookutils.GetSecretName()

	uninit   = make(chan struct{})
	onceInit = sync.Once{}
)

func Inited() chan struct{} {
	return uninit
}

type Controller struct {
	kubeClient clientset.Interface
	handlers   map[string]admission.Handler

	informerFactory informers.SharedInformerFactory
	synced          []cache.InformerSynced
	queue           workqueue.RateLimitingInterface
}

func New(cfg *rest.Config, handlers map[string]admission.Handler) (*Controller, error) {
	kubeClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	c := &Controller{
		kubeClient: kubeClient,
		handlers:   handlers,
		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "webhook-controller"),
	}

	c.informerFactory = informers.NewSharedInformerFactory(c.kubeClient, 0)

	secretInformer := coreinformers.New(c.informerFactory, namespace, nil).Secrets()
	admissionRegistrationInformer := admissionregistrationinformers.New(c.informerFactory, v1.NamespaceAll, nil)

	secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			secret := obj.(*v1.Secret)
			if secret.Name == secretName {
				c.queue.Add("")
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			secret := cur.(*v1.Secret)
			if secret.Name == secretName {
				c.queue.Add("")
			}
		},
	})

	admissionRegistrationInformer.ValidatingWebhookConfigurations().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			conf := obj.(*admissionregistrationv1.ValidatingWebhookConfiguration)
			if conf.Name == validatingWebhookConfigurationName {
				c.queue.Add("")
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			conf := cur.(*admissionregistrationv1.ValidatingWebhookConfiguration)
			if conf.Name == validatingWebhookConfigurationName {
				c.queue.Add("")
			}
		},
	})

	c.synced = []cache.InformerSynced{
		secretInformer.Informer().HasSynced,
		admissionRegistrationInformer.ValidatingWebhookConfigurations().Informer().HasSynced,
	}

	return c, nil
}

func (c *Controller) Start(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	c.informerFactory.Start(ctx.Done())
	if !cache.WaitForNamedCacheSync("webhook-controller", ctx.Done(), c.synced...) {
		return
	}

	go wait.Until(func() {
		for c.processNextWorkItem() {
		}
	}, time.Second, ctx.Done())

	<-ctx.Done()
}

func (c *Controller) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.sync()
	if err == nil {
		c.queue.AddAfter(key, defaultResyncPeriod)
		c.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("sync %q failed with %v", key, err))
	c.queue.AddRateLimited(key)

	return true
}

func (c *Controller) sync() error {
	var dnsName string
	var certWriter writer.CertWriter
	var err error

	if dnsName = webhookutils.GetHost(); len(dnsName) == 0 {
		dnsName = generator.ServiceToCommonName(webhookutils.GetNamespace(), webhookutils.GetServiceName())
	}

	certWriterType := webhookutils.GetCertWriter()
	if certWriterType == writer.FsCertWriter || (len(certWriterType) == 0 && len(webhookutils.GetHost()) != 0) {
		certWriter, err = writer.NewFSCertWriter(writer.FSCertWriterOptions{
			Path: webhookutils.GetCertDir(),
		})
	} else {
		certWriter, err = writer.NewSecretCertWriter(writer.SecretCertWriterOptions{
			Clientset: c.kubeClient,
			Secret:    &types.NamespacedName{Namespace: webhookutils.GetNamespace(), Name: webhookutils.GetSecretName()},
		})
	}
	if err != nil {
		return fmt.Errorf("failed to ensure certs: %v", err)
	}

	certs, _, err := certWriter.EnsureCert(dnsName)
	if err != nil {
		return fmt.Errorf("failed to ensure certs: %v", err)
	}
	if err := writer.WriteCertsToDir(webhookutils.GetCertDir(), certs); err != nil {
		return fmt.Errorf("failed to write certs to dir: %v", err)
	}

	if err := configuration.Ensure(c.kubeClient, c.handlers, certs.CACert); err != nil {
		return fmt.Errorf("failed to ensure configuration: %v", err)
	}

	onceInit.Do(func() {
		close(uninit)
	})
	return nil
}
