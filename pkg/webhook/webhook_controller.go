package webhook

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/openkruise/agents/pkg/utils/webhookutils"
	"github.com/openkruise/agents/pkg/utils/webhookutils/configuration"
	"github.com/openkruise/agents/pkg/utils/webhookutils/generator"
	"github.com/openkruise/agents/pkg/utils/webhookutils/writer"
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
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	validatingWebhookConfigurationName = "kruise-sandbox-operator-validating-webhook-configuration"
	mutatingWebhookConfigurationName   = "kruise-sandbox-operator-mutating-webhook-configuration"

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

	admissionRegistrationInformer.MutatingWebhookConfigurations().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			conf := obj.(*admissionregistrationv1.MutatingWebhookConfiguration)
			if conf.Name == mutatingWebhookConfigurationName {
				klog.Infof("MutatingWebhookConfiguration %s added", mutatingWebhookConfigurationName)
				c.queue.Add("")
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			conf := cur.(*admissionregistrationv1.MutatingWebhookConfiguration)
			if conf.Name == mutatingWebhookConfigurationName {
				klog.Infof("MutatingWebhookConfiguration %s update", mutatingWebhookConfigurationName)
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

	log := klog.FromContext(ctx)
	log.Info("starting webhook controller")

	c.informerFactory.Start(ctx.Done())
	if !cache.WaitForNamedCacheSync("webhook-controller", ctx.Done(), c.synced...) {
		return
	}
	log.Info("informer factory started")

	go wait.Until(func() {
		log.Info("start to process work item")
		for c.processNextWorkItem(ctx) {
		}
	}, time.Second, ctx.Done())

	<-ctx.Done()
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	log := klog.FromContext(ctx)
	key, quit := c.queue.Get()
	log.Info("process next work item", "key", key, "quit", quit)
	if quit {
		return false
	}
	defer c.queue.Done(key)
	log.Info("will do sync")
	err := c.sync(ctx)
	if err == nil {
		log.Info("sync done")
		c.queue.AddAfter(key, defaultResyncPeriod)
		c.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("sync %q failed with %v", key, err))
	c.queue.AddRateLimited(key)

	return true
}

func (c *Controller) sync(ctx context.Context) error {
	log := klog.FromContext(ctx)
	var dnsName string
	var certWriter writer.CertWriter
	var err error

	if dnsName = webhookutils.GetHost(); len(dnsName) == 0 {
		dnsName = generator.ServiceToCommonName(webhookutils.GetNamespace(), webhookutils.GetServiceName())
	}
	log.Info("dns name got", "dnsName", dnsName)

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
	log.Info("ensure certs done")

	if err := writer.WriteCertsToDir(webhookutils.GetCertDir(), certs); err != nil {
		return fmt.Errorf("failed to write certs to dir: %v", err)
	}
	log.Info("write certs to dir")

	if err := configuration.Ensure(c.kubeClient, c.handlers, certs.CACert); err != nil {
		return fmt.Errorf("failed to ensure configuration: %v", err)
	}
	log.Info("ensure configuration done")

	onceInit.Do(func() {
		close(uninit)
	})
	return nil
}
