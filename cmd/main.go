package main

import (
    "context"
    "fmt"
    "io"
    "log"
    "os"
    "os/signal"
    "strconv"
    "strings"
    "sync"
    "syscall"
    "time"

    appsv1 "k8s.io/api/apps/v1"
    policyv1 "k8s.io/api/policy/v1"
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/labels"
    "k8s.io/apimachinery/pkg/util/intstr"
    "k8s.io/client-go/informers"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/cache"
    "k8s.io/client-go/tools/clientcmd"
    "k8s.io/client-go/tools/leaderelection"
    "k8s.io/client-go/tools/leaderelection/resourcelock"
    apierrors "k8s.io/apimachinery/pkg/api/errors"
    "sigs.k8s.io/yaml"
)

const (
    configMapNamespace = "castai-agent"
    configMapName      = "castai-pdb-controller-config"
    annotationMinAvailable   = "workloads.cast.ai/pdb-minAvailable"
    annotationMaxUnavailable = "workloads.cast.ai/pdb-maxUnavailable"
    annotationBypass         = "workloads.cast.ai/bypass-default-pdb"
    defaultLogInterval       = 15 * time.Minute
    defaultPDBScanInterval   = 2 * time.Minute
    defaultGarbageCollectInterval = 2 * time.Minute
    defaultPDBDumpInterval   = 5 * time.Minute
    pdbDumpFile              = "/tmp/castai-pdbs.yaml"
)

type DefaultPDBConfig struct {
    MinAvailable           *intstr.IntOrString
    MaxUnavailable         *intstr.IntOrString
    FixPoorPDBs            bool
    LogInterval            time.Duration
    PDBScanInterval        time.Duration
    GarbageCollectInterval time.Duration
    PDBDumpInterval        time.Duration
}

var (
    defaultPDBConfig     DefaultPDBConfig
    defaultPDBConfigLock sync.RWMutex

    skipLogTimes     = make(map[string]time.Time)
    skipLogTimesLock sync.Mutex
    warnLogTimes     = make(map[string]time.Time)
    warnLogTimesLock sync.Mutex
    fixLogTimes      = make(map[string]time.Time)
    fixLogTimesLock  sync.Mutex
)

// parseDurationFromConfigMap parses a duration string from the ConfigMap (e.g., "5m", "30s") and returns a time.Duration.
// Falls back to the provided default if the value is missing or invalid.
func parseDurationFromConfigMap(data map[string]string, key string, defaultDuration time.Duration) time.Duration {
    if val, ok := data[key]; ok {
        duration, err := time.ParseDuration(val)
        if err != nil {
            log.Printf("Invalid duration for %s in ConfigMap: %v, using default %v", key, err, defaultDuration)
            return defaultDuration
        }
        if duration <= 0 {
            log.Printf("Non-positive duration for %s in ConfigMap: %v, using default %v", key, duration, defaultDuration)
            return defaultDuration
        }
        return duration
    }
    return defaultDuration
}

// logging for FixPoorPDB behavior
func logFixPoorPDB(key, message string) {
    now := time.Now()
    defaultPDBConfigLock.RLock()
    interval := defaultPDBConfig.LogInterval
    defaultPDBConfigLock.RUnlock()
    fixLogTimesLock.Lock()
    last, ok := fixLogTimes[key]
    if !ok || now.Sub(last) > interval {
        log.Print(message)
        fixLogTimes[key] = now
    }
    fixLogTimesLock.Unlock()
}

// logging for WarnPoorPDB behavior
func logPoorPDBWarning(key, message string) {
    now := time.Now()
    defaultPDBConfigLock.RLock()
    interval := defaultPDBConfig.LogInterval
    defaultPDBConfigLock.RUnlock()
    warnLogTimesLock.Lock()
    last, ok := warnLogTimes[key]
    if !ok || now.Sub(last) > interval {
        log.Print(message)
        warnLogTimes[key] = now
    }
    warnLogTimesLock.Unlock()
}

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    config, err := rest.InClusterConfig()
    if err != nil {
        kubeconfig := os.Getenv("KUBECONFIG")
        config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
        if err != nil {
            panic(err)
        }
    }

    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        panic(err)
    }

    id, err := os.Hostname()
    if err != nil {
        panic(err)
    }

    // Silence logs in all pods until leadership is acquired
    log.SetOutput(io.Discard)

    loadDefaultPDBConfig(ctx, clientset)
    go watchConfigMap(ctx, clientset)

    // Scan all existing PDBs for poor configuration at startup
    go scanAllPDBsForPoorConfig(ctx, clientset)

    lock := &resourcelock.LeaseLock{
        LeaseMeta: metav1.ObjectMeta{
            Name:      "castai-pdb-controller-leader-election",
            Namespace: configMapNamespace,
        },
        Client: clientset.CoordinationV1(),
        LockConfig: resourcelock.ResourceLockConfig{
            Identity: id,
        },
    }

    leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
        Lock:            lock,
        LeaseDuration:   15 * time.Second,
        RenewDeadline:   10 * time.Second,
        RetryPeriod:     2 * time.Second,
        Callbacks: leaderelection.LeaderCallbacks{
            OnStartedLeading: func(ctx context.Context) {
                // Re-enable logging for the leader pod
                log.SetOutput(os.Stderr)
                log.Printf("[%s] I am the leader now\n", id)
                runController(ctx, clientset)
            },
            OnStoppedLeading: func() {
                log.Printf("[%s] Lost leadership, exiting\n", id)
                os.Exit(0)
            },
            OnNewLeader: func(identity string) {
                if identity == id {
                    log.Printf("[%s] I am the new leader\n", id)
                } else {
                    log.Printf("[%s] New leader elected: %s\n", id, identity)
                }
            },
        },
        ReleaseOnCancel: true,
        Name:            "castai-pdb-controller",
    })
}

// MinimalPDB is a struct for serializing PDBs with only essential fields.
type MinimalPDB struct {
    APIVersion string            `yaml:"apiVersion"`
    Kind       string            `yaml:"kind"`
    Metadata   MinimalMetadata   `yaml:"metadata"`
    Spec       policyv1.PodDisruptionBudgetSpec `yaml:"spec"`
}

// MinimalMetadata includes only name and namespace for PDB metadata.
type MinimalMetadata struct {
    Name      string `yaml:"name"`
    Namespace string `yaml:"namespace"`
}

// Dumps all castai-created PDBs to a YAML file in /tmp, overwriting on each run.
func dumpCastaiPDBsToFile(ctx context.Context, clientset *kubernetes.Clientset) {
    pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
    if err != nil {
        log.Printf("Failed to list PDBs for dumping to file: %v", err)
        return
    }

    var yamlOutput []string
    for _, pdb := range pdbs.Items {
        if strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb") {
            // Create a minimal PDB object for clean YAML output
            minimalPDB := MinimalPDB{
                APIVersion: "policy/v1",
                Kind:       "PodDisruptionBudget",
                Metadata: MinimalMetadata{
                    Name:      pdb.Name,
                    Namespace: pdb.Namespace,
                },
                Spec:       pdb.Spec,
            }
            pdbYaml, err := yaml.Marshal(&minimalPDB)
            if err != nil {
                log.Printf("Failed to marshal PDB %s/%s to YAML: %v", pdb.Namespace, pdb.Name, err)
                continue
            }
            yamlOutput = append(yamlOutput, string(pdbYaml))
        }
    }

    // Write to file, overwriting any existing content
    output := strings.Join(yamlOutput, "\n---\n")
    err = os.WriteFile(pdbDumpFile, []byte(output), 0644)
    if err != nil {
        log.Printf("Failed to write PDBs to %s: %v", pdbDumpFile, err)
        return
    }
    log.Printf("Successfully wrote %d castai PDBs to %s", len(yamlOutput), pdbDumpFile)
}

// watches when there are configMap changes
func watchConfigMap(ctx context.Context, clientset *kubernetes.Clientset) {
    factory := informers.NewSharedInformerFactoryWithOptions(
        clientset,
        0,
        informers.WithNamespace(configMapNamespace),
    )
    cmInformer := factory.Core().V1().ConfigMaps().Informer()

    cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc: func(obj interface{}) {
            if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == configMapName {
                updateDefaultPDBConfig(cm, clientset)
            }
        },
        UpdateFunc: func(oldObj, newObj interface{}) {
            if cm, ok := newObj.(*corev1.ConfigMap); ok && cm.Name == configMapName {
                updateDefaultPDBConfig(cm, clientset)
            }
        },
        DeleteFunc: func(obj interface{}) {
            if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == configMapName {
                resetDefaultPDBConfig()
            }
        },
    })

    factory.Start(ctx.Done())
    factory.WaitForCacheSync(ctx.Done())
    <-ctx.Done()
}

// makes necessary updates to default behavior
func updateDefaultPDBConfig(cm *corev1.ConfigMap, clientset *kubernetes.Clientset) {
    var minAvailable, maxUnavailable *intstr.IntOrString
    if val, ok := cm.Data["defaultMinAvailable"]; ok {
        minAvailable = parsePDBValue(val)
    }
    if val, ok := cm.Data["defaultMaxUnavailable"]; ok {
        maxUnavailable = parsePDBValue(val)
    }
    fixPoorPDBs := false
    if val, ok := cm.Data["FixPoorPDBs"]; ok && strings.ToLower(val) == "true" {
        fixPoorPDBs = true
    }
    if minAvailable != nil && maxUnavailable != nil {
        log.Printf("Invalid default PDB config: both defaultMinAvailable and defaultMaxUnavailable set in ConfigMap\n")
        minAvailable = nil
        maxUnavailable = nil
    }

    // Parse interval settings from ConfigMap
    logInterval := parseDurationFromConfigMap(cm.Data, "logInterval", defaultLogInterval)
    pdbScanInterval := parseDurationFromConfigMap(cm.Data, "pdbScanInterval", defaultPDBScanInterval)
    garbageCollectInterval := parseDurationFromConfigMap(cm.Data, "garbageCollectInterval", defaultGarbageCollectInterval)
    pdbDumpInterval := parseDurationFromConfigMap(cm.Data, "pdbDumpInterval", defaultPDBDumpInterval)

    defaultPDBConfigLock.Lock()
    defaultPDBConfig.MinAvailable = minAvailable
    defaultPDBConfig.MaxUnavailable = maxUnavailable
    defaultPDBConfig.FixPoorPDBs = fixPoorPDBs
    defaultPDBConfig.LogInterval = logInterval
    defaultPDBConfig.PDBScanInterval = pdbScanInterval
    defaultPDBConfig.GarbageCollectInterval = garbageCollectInterval
    defaultPDBConfig.PDBDumpInterval = pdbDumpInterval
    defaultPDBConfigLock.Unlock()

    log.Printf("Default PDB config updated from ConfigMap: defaultMinAvailable=%v, defaultMaxUnavailable=%v, FixPoorPDBs=%v, "+
        "logInterval=%v, pdbScanInterval=%v, garbageCollectInterval=%v, pdbDumpInterval=%v\n",
        minAvailable, maxUnavailable, fixPoorPDBs, logInterval, pdbScanInterval,
        garbageCollectInterval, pdbDumpInterval)

    // Reconcile all PDBs that use defaults
    go reconcileAllDefaultPDBs(context.Background(), clientset)
    go scanAllPDBsForPoorConfig(context.Background(), clientset)
}

// resets default behavior
func resetDefaultPDBConfig() {
    defaultPDBConfigLock.Lock()
    defaultPDBConfig.MinAvailable = nil
    defaultPDBConfig.MaxUnavailable = nil
    defaultPDBConfig.FixPoorPDBs = false
    defaultPDBConfig.LogInterval = defaultLogInterval
    defaultPDBConfig.PDBScanInterval = defaultPDBScanInterval
    defaultPDBConfig.GarbageCollectInterval = defaultGarbageCollectInterval
    defaultPDBConfig.PDBDumpInterval = defaultPDBDumpInterval
    defaultPDBConfigLock.Unlock()
    log.Printf("Default PDB config reset: using built-in fallback\n")
}

// loads the configmap values
func loadDefaultPDBConfig(ctx context.Context, clientset *kubernetes.Clientset) {
    cm, err := clientset.CoreV1().ConfigMaps(configMapNamespace).Get(ctx, configMapName, metav1.GetOptions{})
    if err != nil {
        log.Printf("Warning: could not load default PDB config, using built-in defaults: %v\n", err)
        return
    }
    updateDefaultPDBConfig(cm, clientset)
}

// Runs the main controller loop to automatically create, update, and delete PodDisruptionBudgets for Deployments and StatefulSets in response to workload events and annotations.
func runController(ctx context.Context, clientset *kubernetes.Clientset) {
    factory := informers.NewSharedInformerFactory(clientset, 10*time.Minute)

    deployInformer := factory.Apps().V1().Deployments().Informer()
    deployInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc:    func(obj interface{}) { createPDBForWorkload(ctx, clientset, obj) },
        UpdateFunc: func(old, new interface{}) { handleWorkloadUpdate(ctx, clientset, old, new) },
        DeleteFunc: func(obj interface{}) { deletePDBForWorkload(ctx, clientset, obj) },
    })

    stsInformer := factory.Apps().V1().StatefulSets().Informer()
    stsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc:    func(obj interface{}) { createPDBForWorkload(ctx, clientset, obj) },
        UpdateFunc: func(old, new interface{}) { handleWorkloadUpdate(ctx, clientset, old, new) },
        DeleteFunc: func(obj interface{}) { deletePDBForWorkload(ctx, clientset, obj) },
    })

    // Periodically scan for poor and multiple PDBs
    go func() {
        defaultPDBConfigLock.RLock()
        interval := defaultPDBConfig.PDBScanInterval
        defaultPDBConfigLock.RUnlock()
        ticker := time.NewTicker(interval)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                defaultPDBConfigLock.RLock()
                newInterval := defaultPDBConfig.PDBScanInterval
                defaultPDBConfigLock.RUnlock()
                if newInterval != interval {
                    ticker.Stop()
                    ticker = time.NewTicker(newInterval)
                    interval = newInterval
                    log.Printf("Updated PDB scan interval to %v", interval)
                }
                scanAllPDBsForPoorConfig(ctx, clientset)
                scanAllPDBsForMultiplePDBs(ctx, clientset)
            case <-ctx.Done():
                return
            }
        }
    }()

    // Periodically garbage collect orphaned PDBs
    go func() {
        defaultPDBConfigLock.RLock()
        interval := defaultPDBConfig.GarbageCollectInterval
        defaultPDBConfigLock.RUnlock()
        garbageCollectOrphanedPDBs(ctx, clientset)
        ticker := time.NewTicker(interval)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                defaultPDBConfigLock.RLock()
                newInterval := defaultPDBConfig.GarbageCollectInterval
                defaultPDBConfigLock.RUnlock()
                if newInterval != interval {
                    ticker.Stop()
                    ticker = time.NewTicker(newInterval)
                    interval = newInterval
                    log.Printf("Updated garbage collect interval to %v", interval)
                }
                garbageCollectOrphanedPDBs(ctx, clientset)
            case <-ctx.Done():
                return
            }
        }
    }()

    // Periodically dump castai PDBs to YAML file
    go func() {
        defaultPDBConfigLock.RLock()
        interval := defaultPDBConfig.PDBDumpInterval
        defaultPDBConfigLock.RUnlock()
        dumpCastaiPDBsToFile(ctx, clientset)
        ticker := time.NewTicker(interval)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                defaultPDBConfigLock.RLock()
                newInterval := defaultPDBConfig.PDBDumpInterval
                defaultPDBConfigLock.RUnlock()
                if newInterval != interval {
                    ticker.Stop()
                    ticker = time.NewTicker(newInterval)
                    interval = newInterval
                    log.Printf("Updated PDB dump interval to %v", interval)
                }
                dumpCastaiPDBsToFile(ctx, clientset)
            case <-ctx.Done():
                return
            }
        }
    }()

    factory.Start(ctx.Done())
    for _, synced := range []cache.InformerSynced{deployInformer.HasSynced, stsInformer.HasSynced} {
        if !cache.WaitForCacheSync(ctx.Done(), synced) {
            panic("failed to sync informer cache")
        }
    }
    <-ctx.Done()
}

// watches for changes to the custom annotations (min, max, and bypass)
func handleWorkloadUpdate(ctx context.Context, clientset *kubernetes.Clientset, oldObj, newObj interface{}) {
    var oldAnnotations, newAnnotations map[string]string
    var namespace, name string

    switch oldWorkload := oldObj.(type) {
    case *appsv1.Deployment:
        oldAnnotations = oldWorkload.Annotations
        newWorkload := newObj.(*appsv1.Deployment)
        newAnnotations = newWorkload.Annotations
        namespace = newWorkload.Namespace
        name = newWorkload.Name
    case *appsv1.StatefulSet:
        oldAnnotations = oldWorkload.Annotations
        newWorkload := newObj.(*appsv1.StatefulSet)
        newAnnotations = newWorkload.Annotations
        namespace = newWorkload.Namespace
        name = newWorkload.Name
    default:
        return
    }

    oldBypass := false
    if oldAnnotations != nil {
        if val, ok := oldAnnotations[annotationBypass]; ok && val == "true" {
            oldBypass = true
        }
    }
    newBypass := false
    if newAnnotations != nil {
        if val, ok := newAnnotations[annotationBypass]; ok && val == "true" {
            newBypass = true
        }
    }

    // If bypass annotation is added, remove PDB
    if !oldBypass && newBypass {
        pdbName := fmt.Sprintf("castai-%s-pdb", name)
        err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdbName, metav1.DeleteOptions{})
        if err != nil && !apierrors.IsNotFound(err) {
            log.Printf("Failed to delete PDB %s/%s after bypass annotation added: %v\n", namespace, pdbName, err)
        } else {
            log.Printf("Bypass annotation added to %s/%s, removed PDB\n", namespace, name)
        }
        return
    }

    // If bypass annotation is removed, create PDB
    if oldBypass && !newBypass {
        log.Printf("Bypass annotation removed from %s/%s, creating PDB if needed\n", namespace, name)
        createPDBForWorkload(ctx, clientset, newObj)
        return
    }

    // If bypass annotation is present, do not create PDB
    if newBypass {
        return
    }

    // Check if minAvailable or maxUnavailable annotation changed
    oldMin := ""
    newMin := ""
    oldMax := ""
    newMax := ""
    if oldAnnotations != nil {
        oldMin = oldAnnotations[annotationMinAvailable]
        oldMax = oldAnnotations[annotationMaxUnavailable]
    }
    if newAnnotations != nil {
        newMin = newAnnotations[annotationMinAvailable]
        newMax = newAnnotations[annotationMaxUnavailable]
    }
    if oldMin != newMin || oldMax != newMax {
        log.Printf("PDB annotation changed for %s/%s, updating PDB\n", namespace, name)
        createPDBForWorkload(ctx, clientset, newObj)
        return
    }

    // For all other updates, run normal logic
    createPDBForWorkload(ctx, clientset, newObj)
}

// selectorMatchesLabelsFull supports both matchLabels and matchExpressions.
func selectorMatchesLabelsFull(selector *metav1.LabelSelector, lbls map[string]string) (bool, error) {
    if selector == nil {
        return false, nil
    }
    sel, err := metav1.LabelSelectorAsSelector(selector)
    if err != nil {
        return false, err
    }
    return sel.Matches(labels.Set(lbls)), nil
}

// helps understand the int vs % inputs for the PDB values
func parsePDBValue(input string) *intstr.IntOrString {
    input = strings.TrimSpace(input)
    if strings.HasSuffix(input, "%") {
        return &intstr.IntOrString{Type: intstr.String, StrVal: input}
    }
    if val, err := strconv.Atoi(input); err == nil {
        return &intstr.IntOrString{Type: intstr.Int, IntVal: int32(val)}
    }
    return nil
}

// controls the poor PDB logging behavior, moves to createPDB function if configMap set to True
func logAndFixPoorPDBConfig(
    ctx context.Context,
    clientset *kubernetes.Clientset,
    pdb *policyv1.PodDisruptionBudget,
    workloadName string,
    replicas int32,
    namespace string,
    workloadObj interface{},
) {
    defaultPDBConfigLock.RLock()
    fixPoor := defaultPDBConfig.FixPoorPDBs
    defaultPDBConfigLock.RUnlock()

    poorConfig := false

    if pdb.Spec.MinAvailable != nil {
        min := pdb.Spec.MinAvailable
        if min.Type == intstr.Int && min.IntVal == replicas {
            key := fmt.Sprintf("%s/%s/minavailable-equals-replicas", namespace, pdb.Name)
            msg := fmt.Sprintf("WARNING: PDB %s/%s has minAvailable equal to replica count (%d). This is overly restrictive and may block disruptions.", namespace, pdb.Name, replicas)
            logPoorPDBWarning(key, msg)
            poorConfig = true
        }
        if min.Type == intstr.String && strings.TrimSpace(min.StrVal) == "100%" {
            key := fmt.Sprintf("%s/%s/minavailable-100-percent", namespace, pdb.Name)
            msg := fmt.Sprintf("WARNING: PDB %s/%s has minAvailable set to 100%%. This is overly restrictive and may block disruptions.", namespace, pdb.Name)
            logPoorPDBWarning(key, msg)
            poorConfig = true
        }
    }

    if pdb.Spec.MaxUnavailable != nil {
        max := pdb.Spec.MaxUnavailable
        if max.Type == intstr.Int && max.IntVal == 0 {
            key := fmt.Sprintf("%s/%s/maxunavailable-zero", namespace, pdb.Name)
            msg := fmt.Sprintf("WARNING: PDB %s/%s has maxUnavailable set to 0. This is overly restrictive and may block disruptions.", namespace, pdb.Name)
            logPoorPDBWarning(key, msg)
            poorConfig = true
        }
        if max.Type == intstr.String && strings.TrimSpace(max.StrVal) == "0%" {
            key := fmt.Sprintf("%s/%s/maxunavailable-zero-percent", namespace, pdb.Name)
            msg := fmt.Sprintf("WARNING: PDB %s/%s has maxUnavailable set to 0%%. This is overly restrictive and may block disruptions.", namespace, pdb.Name)
            logPoorPDBWarning(key, msg)
            poorConfig = true
        }
    }

    // Bypass annotation check
    if poorConfig && fixPoor && workloadObj != nil {
        var annotations map[string]string
        switch w := workloadObj.(type) {
        case *appsv1.Deployment:
            annotations = w.Annotations
        case *appsv1.StatefulSet:
            annotations = w.Annotations
        }
        if annotations != nil {
            if val, ok := annotations[annotationBypass]; ok && val == "true" {
                // Respect bypass annotation: do not delete or recreate PDB
                return
            }
        }

        key := fmt.Sprintf("%s/%s", namespace, pdb.Name)
        msg := fmt.Sprintf("FixPoorPDBs enabled: Deleting poor PDB %s/%s and recreating with defaults.", namespace, pdb.Name)
        logFixPoorPDB(key, msg)
        err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdb.Name, metav1.DeleteOptions{})
        if err != nil && !apierrors.IsNotFound(err) {
            log.Printf("Failed to delete poor PDB %s/%s: %v\n", namespace, pdb.Name, err)
            return
        }
        // Recreate PDB using the default creation flow for the workload
        createPDBForWorkload(ctx, clientset, workloadObj)
    }
}

// core create PDB workflow
func createPDBForWorkload(ctx context.Context, clientset *kubernetes.Clientset, obj interface{}) {
    var (
        selector            *metav1.LabelSelector
        namespace, name     string
        workloadAnnotations map[string]string
        replicas            *int32
    )

    switch workload := obj.(type) {
    case *appsv1.Deployment:
        replicas = workload.Spec.Replicas
        if replicas == nil || *replicas < 2 {
            return
        }
        if workload.Annotations != nil {
            if val, ok := workload.Annotations[annotationBypass]; ok && val == "true" {
                return
            }
        }
        selector = workload.Spec.Selector
        workloadAnnotations = workload.Annotations
        namespace = workload.Namespace
        name = workload.Name
    case *appsv1.StatefulSet:
        replicas = workload.Spec.Replicas
        if replicas == nil || *replicas < 2 {
            return
        }
        if workload.Annotations != nil {
            if val, ok := workload.Annotations[annotationBypass]; ok && val == "true" {
                return
            }
        }
        selector = workload.Spec.Selector
        workloadAnnotations = workload.Annotations
        namespace = workload.Namespace
        name = workload.Name
    default:
        return
    }

    if selector == nil {
        return
    }

    workloadSel, err := metav1.LabelSelectorAsSelector(selector)
    if err != nil {
        log.Printf("Invalid selector for %s/%s: %v\n", namespace, name, err)
        return
    }

    pdbList, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
    if err != nil {
        log.Printf("Failed to list PDBs in namespace %s: %v\n", namespace, err)
        return
    }

    foundNonPoorMatchingPDB := false
    foundPoorMatchingPDB := false
    fixPoor := false
    defaultPDBConfigLock.RLock()
    fixPoor = defaultPDBConfig.FixPoorPDBs
    logInterval := defaultPDBConfig.LogInterval
    defaultPDBConfigLock.RUnlock()

    for _, pdb := range pdbList.Items {
        if pdb.Spec.Selector != nil {
            pdbSel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
            if err == nil && workloadSel.String() == pdbSel.String() {
                poorConfig := false
                if replicas != nil {
                    poorConfig = isPoorPDBConfig(&pdb, *replicas)
                }
                if poorConfig {
                    if fixPoor && replicas != nil {
                        logAndFixPoorPDBConfig(ctx, clientset, &pdb, name, *replicas, namespace, obj)
                    } else {
                        logAndFixPoorPDBConfig(ctx, clientset, &pdb, name, *replicas, namespace, obj)
                        foundPoorMatchingPDB = true
                    }
                } else {
                    key := fmt.Sprintf("%s/%s", namespace, name)
                    now := time.Now()
                    skipLogTimesLock.Lock()
                    last, ok := skipLogTimes[key]
                    if !ok || now.Sub(last) > logInterval {
                        log.Printf("Skipping PDB creation for %s/%s: overlapping PDB %s exists\n", namespace, name, pdb.Name)
                        skipLogTimes[key] = now
                    }
                    skipLogTimesLock.Unlock()
                    foundNonPoorMatchingPDB = true
                }
            }
        }
    }

    if foundNonPoorMatchingPDB || foundPoorMatchingPDB {
        return
    }
    time.Sleep(3 * time.Second)

    // Double-check for a matching PDB after the delay
    pdbListAfter, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
    if err == nil {
        for _, pdb := range pdbListAfter.Items {
            if pdb.Spec.Selector != nil {
                pdbSel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
                if err == nil && workloadSel.String() == pdbSel.String() {
                    return
                }
            }
        }
    }

    pdbName := fmt.Sprintf("castai-%s-pdb", name)
    existingPDB, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Get(ctx, pdbName, metav1.GetOptions{})
    exists := err == nil
    if err != nil && !apierrors.IsNotFound(err) {
        log.Printf("Error checking for existing PDB %s/%s: %v\n", namespace, pdbName, err)
        return
    }

    var minAvailable *intstr.IntOrString
    var maxUnavailable *intstr.IntOrString

    if workloadAnnotations != nil {
        if val, ok := workloadAnnotations[annotationMinAvailable]; ok {
            minAvailable = parsePDBValue(val)
        }
        if val, ok := workloadAnnotations[annotationMaxUnavailable]; ok {
            maxUnavailable = parsePDBValue(val)
        }
    }

    if minAvailable == nil && maxUnavailable == nil {
        defaultPDBConfigLock.RLock()
        minAvailable = defaultPDBConfig.MinAvailable
        maxUnavailable = defaultPDBConfig.MaxUnavailable
        defaultPDBConfigLock.RUnlock()
    }

    if minAvailable != nil && maxUnavailable != nil {
        log.Printf("Invalid PDB spec for %s/%s: both pdb-minAvailable and pdb-maxUnavailable set\n", namespace, name)
        return
    }

    if minAvailable == nil && maxUnavailable == nil {
        minAvailable = &intstr.IntOrString{Type: intstr.Int, IntVal: 1}
    }

    pdbSpec := policyv1.PodDisruptionBudgetSpec{
        Selector: selector.DeepCopy(), // Use the full selector (labels + expressions)
    }
    if minAvailable != nil {
        pdbSpec.MinAvailable = minAvailable
    }
    if maxUnavailable != nil {
        pdbSpec.MaxUnavailable = maxUnavailable
    }

    if exists {
        needsUpdate := false
        if existingPDB.Spec.MinAvailable == nil && pdbSpec.MinAvailable != nil {
            needsUpdate = true
        } else if existingPDB.Spec.MinAvailable != nil && pdbSpec.MinAvailable == nil {
            needsUpdate = true
        } else if existingPDB.Spec.MinAvailable != nil && pdbSpec.MinAvailable != nil &&
            existingPDB.Spec.MinAvailable.String() != pdbSpec.MinAvailable.String() {
            needsUpdate = true
        }
        if existingPDB.Spec.MaxUnavailable == nil && pdbSpec.MaxUnavailable != nil {
            needsUpdate = true
        } else if existingPDB.Spec.MaxUnavailable != nil && pdbSpec.MaxUnavailable == nil {
            needsUpdate = true
        } else if existingPDB.Spec.MaxUnavailable != nil && pdbSpec.MaxUnavailable != nil &&
            existingPDB.Spec.MaxUnavailable.String() != pdbSpec.MaxUnavailable.String() {
            needsUpdate = true
        }
        if needsUpdate {
            existingPDB.Spec = pdbSpec
            _, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Update(ctx, existingPDB, metav1.UpdateOptions{})
            if err != nil {
                log.Printf("Failed to update PDB for %s/%s: %v\n", namespace, name, err)
            } else {
                log.Printf("Updated PDB for %s/%s\n", namespace, name)
                if replicas != nil {
                    logAndFixPoorPDBConfig(ctx, clientset, existingPDB, name, *replicas, namespace, obj)
                }
            }
        }
        return
    }

    pdb := &policyv1.PodDisruptionBudget{
        ObjectMeta: metav1.ObjectMeta{
            Name:      pdbName,
            Namespace: namespace,
        },
        Spec: pdbSpec,
    }

    _, err = clientset.PolicyV1().PodDisruptionBudgets(namespace).Create(ctx, pdb, metav1.CreateOptions{})
    if err != nil {
        if apierrors.IsAlreadyExists(err) {
            return
        }
        log.Printf("Failed to create PDB for %s/%s: %v\n", namespace, name, err)
    } else {
        log.Printf("No existing PDB for %s/%s, created PDB %s\n", namespace, name, pdbName)
        if replicas != nil {
            logAndFixPoorPDBConfig(ctx, clientset, pdb, name, *replicas, namespace, obj)
        }
    }
}

// Deletes the PodDisruptionBudget associated with a given Deployment or StatefulSet workload.
func deletePDBForWorkload(ctx context.Context, clientset *kubernetes.Clientset, obj interface{}) {
    var namespace, name string

    switch workload := obj.(type) {
    case *appsv1.Deployment:
        namespace = workload.Namespace
        name = workload.Name
    case *appsv1.StatefulSet:
        namespace = workload.Namespace
        name = workload.Name
    default:
        log.Printf("deletePDBForWorkload: unsupported workload type %T", obj)
        return
    }

    pdbName := fmt.Sprintf("castai-%s-pdb", name)
    err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdbName, metav1.DeleteOptions{})
    if err != nil {
        if apierrors.IsNotFound(err) {
            log.Printf("PDB %s/%s not found, nothing to delete", namespace, pdbName)
        } else {
            log.Printf("Failed to delete PDB %s/%s: %v", namespace, pdbName, err)
        }
    } else {
        log.Printf("Deleted PDB %s/%s", namespace, pdbName)
    }
}

// Deletes orphaned PodDisruptionBudgets that no longer have an associated Deployment or StatefulSet.
func garbageCollectOrphanedPDBs(ctx context.Context, clientset *kubernetes.Clientset) {
    pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
    if err != nil {
        log.Printf("Failed to list PDBs: %v", err)
        return
    }

    for _, pdb := range pdbs.Items {
        if !strings.HasPrefix(pdb.Name, "castai-") || !strings.HasSuffix(pdb.Name, "-pdb") {
            continue
        }
        workloadName := strings.TrimSuffix(strings.TrimPrefix(pdb.Name, "castai-"), "-pdb")

        // Check for existence of Deployment and StatefulSet
        _, errDep := clientset.AppsV1().Deployments(pdb.Namespace).Get(ctx, workloadName, metav1.GetOptions{})
        _, errSts := clientset.AppsV1().StatefulSets(pdb.Namespace).Get(ctx, workloadName, metav1.GetOptions{})

        if apierrors.IsNotFound(errDep) && apierrors.IsNotFound(errSts) {
            err := clientset.PolicyV1().PodDisruptionBudgets(pdb.Namespace).Delete(ctx, pdb.Name, metav1.DeleteOptions{})
            if err != nil {
                if apierrors.IsNotFound(err) {
                    log.Printf("Orphaned PDB %s/%s already deleted", pdb.Namespace, pdb.Name)
                } else {
                    log.Printf("Failed to garbage collect orphaned PDB %s/%s: %v", pdb.Namespace, pdb.Name, err)
                }
            } else {
                log.Printf("Garbage collected orphaned PDB %s/%s", pdb.Namespace, pdb.Name)
            }
        }
    }
}

// Reconcile all workloads that use default PDB config
func reconcileAllDefaultPDBs(ctx context.Context, clientset *kubernetes.Clientset) {
    namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
    if err != nil {
        log.Printf("Failed to list namespaces: %v", err)
        return
    }

    for _, ns := range namespaces.Items {
        // Check Deployments
        deployments, err := clientset.AppsV1().Deployments(ns.Name).List(ctx, metav1.ListOptions{})
        if err != nil {
            log.Printf("Failed to list Deployments in namespace %s: %v", ns.Name, err)
        } else {
            for _, d := range deployments.Items {
                if !hasCustomPDBAnnotations(d.Annotations) &&
                    !hasBypassAnnotation(d.Annotations) &&
                    d.Spec.Replicas != nil && *d.Spec.Replicas >= 2 {
                    createPDBForWorkload(ctx, clientset, &d)
                }
            }
        }

        // Check StatefulSets
        statefulsets, err := clientset.AppsV1().StatefulSets(ns.Name).List(ctx, metav1.ListOptions{})
        if err != nil {
            log.Printf("Failed to list StatefulSets in namespace %s: %v", ns.Name, err)
        } else {
            for _, s := range statefulsets.Items {
                if !hasCustomPDBAnnotations(s.Annotations) &&
                    !hasBypassAnnotation(s.Annotations) &&
                    s.Spec.Replicas != nil && *s.Spec.Replicas >= 2 {
                    createPDBForWorkload(ctx, clientset, &s)
                }
            }
        }
    }
}

// Returns true if the annotations specify custom minAvailable or maxUnavailable for the PDB.
func hasCustomPDBAnnotations(annotations map[string]string) bool {
    if annotations == nil {
        return false
    }
    if _, ok := annotations[annotationMinAvailable]; ok {
        return true
    }
    if _, ok := annotations[annotationMaxUnavailable]; ok {
        return true
    }
    return false
}

// Returns true if the annotations specify that PDB management should be bypassed.
func hasBypassAnnotation(annotations map[string]string) bool {
    if annotations == nil {
        return false
    }
    if val, ok := annotations[annotationBypass]; ok && val == "true" {
        return true
    }
    return false
}

// Audits all PodDisruptionBudgets and logs or fixes those with poor configuration for associated workloads.
func scanAllPDBsForPoorConfig(ctx context.Context, clientset *kubernetes.Clientset) {
    pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
    if err != nil {
        log.Printf("Failed to list PDBs for audit: %v", err)
        return
    }
    for _, pdb := range pdbs.Items {
        if pdb.Spec.Selector == nil {
            continue
        }
        pdbSelector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
        if err != nil {
            log.Printf("Invalid label selector for PDB %s/%s: %v", pdb.Namespace, pdb.Name, err)
            continue
        }

        // Check Deployments
        deployments, err := clientset.AppsV1().Deployments(pdb.Namespace).List(ctx, metav1.ListOptions{})
        if err != nil {
            log.Printf("Failed to list Deployments in namespace %s: %v", pdb.Namespace, err)
        } else {
            for _, deploy := range deployments.Items {
                // Skip if bypass annotation is set
                if deploy.Annotations != nil && deploy.Annotations[annotationBypass] == "true" {
                    continue
                }
                if deploy.Spec.Selector == nil {
                    continue
                }
                workloadSelector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
                if err != nil {
                    log.Printf("Invalid selector for Deployment %s/%s: %v", deploy.Namespace, deploy.Name, err)
                    continue
                }
                // Compare selectors for semantic equivalence
                if pdbSelector.String() == workloadSelector.String() {
                    replicas := int32(1)
                    if deploy.Spec.Replicas != nil {
                        replicas = *deploy.Spec.Replicas
                    }
                    logAndFixPoorPDBConfig(ctx, clientset, &pdb, deploy.Name, replicas, pdb.Namespace, &deploy)
                }
            }
        }

        // Check StatefulSets
        statefulsets, err := clientset.AppsV1().StatefulSets(pdb.Namespace).List(ctx, metav1.ListOptions{})
        if err != nil {
            log.Printf("Failed to list StatefulSets in namespace %s: %v", pdb.Namespace, err)
        } else {
            for _, sts := range statefulsets.Items {
                // Skip if bypass annotation is set
                if sts.Annotations != nil && sts.Annotations[annotationBypass] == "true" {
                    continue
                }
                if sts.Spec.Selector == nil {
                    continue
                }
                workloadSelector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
                if err != nil {
                    log.Printf("Invalid selector for StatefulSet %s/%s: %v", sts.Namespace, sts.Name, err)
                    continue
                }
                // Compare selectors for semantic equivalence
                if pdbSelector.String() == workloadSelector.String() {
                    replicas := int32(1)
                    if sts.Spec.Replicas != nil {
                        replicas = *sts.Spec.Replicas
                    }
                    logAndFixPoorPDBConfig(ctx, clientset, &pdb, sts.Name, replicas, pdb.Namespace, &sts)
                }
            }
        }
    }
}

// Returns true if the PDB configuration is overly restrictive and may block disruptions.
func isPoorPDBConfig(pdb *policyv1.PodDisruptionBudget, replicas int32) bool {
    if pdb.Spec.MinAvailable != nil {
        min := pdb.Spec.MinAvailable
        if (min.Type == intstr.Int && min.IntVal == replicas) ||
           (min.Type == intstr.String && strings.TrimSpace(min.StrVal) == "100%") {
            return true
        }
    }
    if pdb.Spec.MaxUnavailable != nil {
        max := pdb.Spec.MaxUnavailable
        if (max.Type == intstr.Int && max.IntVal == 0) ||
           (max.Type == intstr.String && strings.TrimSpace(max.StrVal) == "0%") {
            return true
        }
    }
    return false
}

// Scans for workloads targeted by multiple PDBs and removes redundant castai PDBs if necessary.
func scanAllPDBsForMultiplePDBs(ctx context.Context, clientset *kubernetes.Clientset) {
    namespaces, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
    if err != nil {
        log.Printf("Failed to list namespaces: %v", err)
        return
    }

    for _, ns := range namespaces.Items {
        namespace := ns.Name

        // Get all PDBs in this namespace
        pdbList, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
        if err != nil {
            log.Printf("Failed to list PDBs in namespace %s: %v", namespace, err)
            continue
        }

        // Check Deployments
        deployments, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
        if err != nil {
            log.Printf("Failed to list Deployments in namespace %s: %v", namespace, err)
        } else {
            for _, deploy := range deployments.Items {
                if deploy.Annotations != nil && deploy.Annotations[annotationBypass] == "true" {
                    continue
                }
                if deploy.Spec.Selector == nil {
                    continue
                }
                workloadSelector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
                if err != nil {
                    log.Printf("Invalid selector for Deployment %s/%s: %v", deploy.Namespace, deploy.Name, err)
                    continue
                }
                matchingPDBs := []*policyv1.PodDisruptionBudget{}
                for i, pdb := range pdbList.Items {
                    if pdb.Spec.Selector == nil {
                        continue
                    }
                    pdbSelector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
                    if err != nil {
                        continue
                    }
                    if pdbSelector.String() == workloadSelector.String() {
                        matchingPDBs = append(matchingPDBs, &pdbList.Items[i])
                    }
                }
                if len(matchingPDBs) > 1 {
                    // Count non-castai PDBs
                    nonCastaiCount := 0
                    for _, pdb := range matchingPDBs {
                        if !(strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb")) {
                            nonCastaiCount++
                        }
                    }
                    // Only delete castai-*-pdb if at least one non-castai PDB exists
                    if nonCastaiCount > 0 {
                        for _, pdb := range matchingPDBs {
                            if strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb") {
                                err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdb.Name, metav1.DeleteOptions{})
                                if err != nil && !apierrors.IsNotFound(err) {
                                    log.Printf("Failed to delete castai PDB %s/%s: %v", namespace, pdb.Name, err)
                                } else {
                                    log.Printf("Deleted castai PDB %s/%s due to multiple PDBs targeting deployment %s", namespace, pdb.Name, deploy.Name)
                                }
                            }
                        }
                    }
                    // Warn if more than one non-castai PDB remains
                    if nonCastaiCount > 1 {
                        log.Printf("WARNING: Multiple non-castai PDBs target deployment %s/%s", namespace, deploy.Name)
                    }
                }
            }
        }

        // Check StatefulSets
        statefulsets, err := clientset.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
        if err != nil {
            log.Printf("Failed to list StatefulSets in namespace %s: %v", namespace, err)
        } else {
            for _, sts := range statefulsets.Items {
                if sts.Annotations != nil && sts.Annotations[annotationBypass] == "true" {
                    continue
                }
                if sts.Spec.Selector == nil {
                    continue
                }
                workloadSelector, err := metav1.LabelSelectorAsSelector(sts.Spec.Selector)
                if err != nil {
                    log.Printf("Invalid selector for StatefulSet %s/%s: %v", sts.Namespace, sts.Name, err)
                    continue
                }
                matchingPDBs := []*policyv1.PodDisruptionBudget{}
                for i, pdb := range pdbList.Items {
                    if pdb.Spec.Selector == nil {
                        continue
                    }
                    pdbSelector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
                    if err != nil {
                        continue
                    }
                    if pdbSelector.String() == workloadSelector.String() {
                        matchingPDBs = append(matchingPDBs, &pdbList.Items[i])
                    }
                }
                if len(matchingPDBs) > 1 {
                    // Count non-castai PDBs
                    nonCastaiCount := 0
                    for _, pdb := range matchingPDBs {
                        if !(strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb")) {
                            nonCastaiCount++
                        }
                    }
                    // Only delete castai-*-pdb if at least one non-castai PDB exists
                    if nonCastaiCount > 0 {
                        for _, pdb := range matchingPDBs {
                            if strings.HasPrefix(pdb.Name, "castai-") && strings.HasSuffix(pdb.Name, "-pdb") {
                                err := clientset.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, pdb.Name, metav1.DeleteOptions{})
                                if err != nil && !apierrors.IsNotFound(err) {
                                    log.Printf("Failed to delete castai PDB %s/%s: %v", namespace, pdb.Name, err)
                                } else {
                                    log.Printf("Deleted castai PDB %s/%s due to multiple PDBs targeting statefulset %s", namespace, pdb.Name, sts.Name)
                                }
                            }
                        }
                    }
                    // Warn if more than one non-castai PDB remains
                    if nonCastaiCount > 1 {
                        log.Printf("WARNING: Multiple non-castai PDBs target statefulset %s/%s", namespace, sts.Name)
                    }
                }
            }
        }
    }
}