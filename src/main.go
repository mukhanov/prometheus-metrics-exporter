package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/client"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultMetricsReadTimeout = 5000 * time.Millisecond

var (
	log                *zap.SugaredLogger
	containersMap      *sync.Map
	metricsReadTimeout time.Duration = -1

	httpClient *http.Client

	removeAllSlashesRE     *regexp.Regexp
	underscoreEverythingRE *regexp.Regexp

	networkAliasNotFoundError        = errors.New("network alias not found")
	invalidEndpointResponseCodeError = errors.New("invalid endpoint response code")
)

type (
	ContainerInfo struct {
		Id                 string
		ContainerName      string
		ServiceName        string
		MetricsEndpointUrl string
	}
)

func init() {

	readTimeoutStr := os.Getenv("METRICS_READ_TIMEOUT")
	if len(readTimeoutStr) == 0 {
		parsed, err := strconv.ParseInt(readTimeoutStr, 10, 64)
		if err == nil {
			metricsReadTimeout = time.Duration(int64(time.Millisecond) * parsed)
		}
	}

	if metricsReadTimeout == -1 {
		metricsReadTimeout = defaultMetricsReadTimeout
	}

	removeAllSlashesRE, _ = regexp.Compile("[/]*")
	underscoreEverythingRE, _ = regexp.Compile("[\\W]+")

	httpClient = &http.Client{}

	config := zap.NewProductionEncoderConfig()
	config.EncodeTime = zapcore.ISO8601TimeEncoder
	logger := zap.New(
		zapcore.NewTee(
			zapcore.NewCore(
				zapcore.NewConsoleEncoder(config),
				zapcore.AddSync(os.Stdout),
				zapcore.DebugLevel,
			),
		),
		zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel),
	)
	log = logger.Sugar()

	containersMap = &sync.Map{}
}

func main() {
	docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Panicf("Unable to connect to docker. Error: %v", err)
	}

	eventsChan, _ := docker.Events(context.Background(), types.EventsOptions{})

	go func() {
		for event := range eventsChan {
			if event.Type != events.ContainerEventType {
				continue
			}
			handleContainerStatusChanged(context.Background(), docker, event.Actor.ID)
		}
	}()

	containers, err := docker.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		log.Panicf("Unable to list containers. Error: %v", err)
	}

	for _, container := range containers {
		handleContainerStatusChanged(context.Background(), docker, container.ID)
	}

	http.HandleFunc("/metrics", Handle)

	_ = http.ListenAndServe(":9999", nil)
}

func handleContainerStatusChanged(ctx context.Context, cli *client.Client, containerID string) {
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		log.Errorf("Unable to inspect container. Error: %v", err)
		return
	}
	if inspect.State.Running {
		addContainer(inspect)
	} else {
		removeContainer(inspect.ID)
	}
}

func getContainerMetricsEndpointUrl(container types.ContainerJSON) (string, error) {

	labels := container.Config.Labels

	protocol, hasProtocol := labels["prometheus.protocol"]
	if !hasProtocol {
		protocol = "http"
	}

	port, hasPort := labels["prometheus.port"]
	if !hasPort {
		port = "9090"
	}

	ctx, hasContext := labels["prometheus.context"]
	if !hasContext {
		ctx = "/metrics"
	}

	//return protocol + "://" + "localhost" + ":" + port + ctx, nil
	for _, settings := range container.NetworkSettings.Networks {
		return protocol + "://" + settings.IPAddress + ":" + port + ctx, nil
	}

	return "", networkAliasNotFoundError
}

func addContainer(container types.ContainerJSON) {

	if ok, _ := strconv.ParseBool(container.Config.Labels["prometheus.enable"]); ok {

		endpointUrl, err := getContainerMetricsEndpointUrl(container)
		if err != nil {
			log.Errorf("Unable to find metrics endpoint url for container [image:%v;id:%v]. Error: [%v]", container.Config.Image, container.ID, err)
			return
		}

		log.Infof("Add container [image:%v;id:%v] with metrics endpoint url [%v]", container.Config.Image, container.ID, endpointUrl)
		containersMap.Store(container.ID, ContainerInfo{
			Id:                 container.ID,
			ServiceName:        createServiceName(container),
			ContainerName:      container.Name,
			MetricsEndpointUrl: endpointUrl,
		})
	}
}

func removeContainer(id string) {
	containersMap.Delete(id)
}

func Handle(resp http.ResponseWriter, _ *http.Request) {

	responseMap := &sync.Map{}

	ctx, cancel := context.WithTimeout(context.Background(), metricsReadTimeout)
	defer cancel()

	wg := sync.WaitGroup{}

	containersMap.Range(func(key, value any) bool {

		var c = value.(ContainerInfo)

		wg.Add(1)

		go func() {
			defer wg.Done()

			err := readServiceMetrics(ctx, c, responseMap)
			if err != nil {
				log.Errorf("Unable to read [containerName:%v;containerId:%v;serviceName:%v] metrics. Error: %v", c.ContainerName, c.Id, c.ServiceName, err)
			}
		}()

		return true
	})

	wg.Wait()

	responseMap.Range(func(key, value any) bool {
		containerInfo := key.(ContainerInfo)
		serviceOutput := fmt.Sprint(value)

		result := transformServiceMetricsResponse(containerInfo, serviceOutput)

		_, err := resp.Write(result)
		if err != nil {
			log.Errorf("Unable to write metrics response. Error: %v", err)
			return false
		}

		return true
	})
}

func readServiceMetrics(ctx context.Context, containerInfo ContainerInfo, responseMap *sync.Map) error {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, containerInfo.MetricsEndpointUrl, nil)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return invalidEndpointResponseCodeError
	}

	buf := new(strings.Builder)

	_, err = io.Copy(buf, resp.Body)
	if err != nil {
		return err
	}

	responseMap.Store(containerInfo, buf.String())

	return nil
}

func transformServiceMetricsResponse(containerInfo ContainerInfo, serviceOutput string) []byte {

	scanner := bufio.NewScanner(strings.NewReader(serviceOutput))

	var buffer bytes.Buffer

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "# HELP"):
			buffer.WriteString(line)
		case strings.HasPrefix(line, "# TYPE"):
			buffer.WriteString(line)
		default:
			buffer.WriteString(transformMetricValueLine(containerInfo, line))
		}
		buffer.WriteRune('\n')
	}

	return buffer.Bytes()
}

func transformMetricValueLine(container ContainerInfo, line string) string {

	metric, tags, value := extractLineData(line)

	tags["service"] = "\"" + container.ServiceName + "\""
	tags["container"] = "\"" + container.ContainerName + "\""

	return createLine(metric, tags, value)
}

func createLine(metric string, tags map[string]string, value string) string {
	res := metric + "{"

	first := true
	for k, v := range tags {
		if !first {
			res += ","
		} else {
			first = false
		}
		res += k + "=" + v
	}

	res += "} " + value
	return res
}

func extractLineData(line string) (string, map[string]string, string) {

	metricValue := ""
	metricName := ""
	tags := make(map[string]string)

	if strings.Contains(line, "{") {
		a := strings.SplitN(line, "} ", 2)
		metricValue = a[1]

		nameAndTags := strings.Split(a[0], "{")

		metricName = nameAndTags[0]

		labelsString := nameAndTags[1]

		labelsWithValues := strings.Split(labelsString, ",")

		for _, labelWithValue := range labelsWithValues {
			kv := strings.Split(labelWithValue, "=")
			tags[kv[0]] = kv[1]
		}

	} else {
		a := strings.Split(line, " ")
		metricName = a[0]
		if len(a) > 1 {
			metricValue = a[1]
		}
	}
	return metricName, tags, metricValue
}

func createServiceName(container types.ContainerJSON) string {

	st1 := removeAllSlashesRE.ReplaceAll([]byte(strings.ToLower(container.Name)), []byte(""))

	st2 := underscoreEverythingRE.ReplaceAll(st1, []byte("_"))

	return string(st2)
}
