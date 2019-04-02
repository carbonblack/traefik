package middlewares

import (
	"encoding/base64"
	"encoding/json"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/defaults"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/containous/traefik/middlewares/tracing"
	"github.com/containous/traefik/log"
	"io/ioutil"
	"net/http"
	"strconv"
	"reflect"
)

// Lambda
type Lambda struct {
	next http.Handler
}

// NewLambda
func NewLambda(next http.Handler) *Lambda {
	return &Lambda{
		next: next,
	}
}

func (l *Lambda) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	sess, err := session.NewSession()
	if err != nil {
		return
	}
	ec2meta := ec2metadata.New(sess)
	identity, err := ec2meta.GetInstanceIdentityDocument()

	cfg := &aws.Config{
		Region: &identity.Region,
		Credentials: credentials.NewChainCredentials(
			[]credentials.Provider{
				&credentials.StaticProvider{
					Value: credentials.Value{
						AccessKeyID:     "",
						SecretAccessKey: "",
					},
				},
				&credentials.EnvProvider{},
				&credentials.SharedCredentialsProvider{},
				defaults.RemoteCredProvider(*(defaults.Config()), defaults.Handlers()),
			}),
	}

	svc := lambda.New(sess, cfg)
	body, err := ioutil.ReadAll(r.Body)
	jsonString, _ := json.Marshal(
		map[string]map[string]string{
			"custom": {
				"X-Request-Context":         r.Header.Get("X-Request-Context"),
				"X-User-Context":            r.Header.Get("X-User-Context"),
				"Cookie":                    r.Header.Get("Cookie"),
				"X-Auth-Token":              r.Header.Get("X-Auth-Token"),
				"X-Original-Request-Method": r.Method,
				"X-Original-Request-Url":    r.RequestURI,
			},
		},
	)
	userContext := string(base64.StdEncoding.EncodeToString([]byte(jsonString)))
	input := &lambda.InvokeInput{
		FunctionName:   aws.String(r.URL.Host),
		InvocationType: aws.String("RequestResponse"),
		Payload:        []byte(body),
		ClientContext:  &userContext,
	}
	req, resp := svc.InvokeRequest(input)
	err = req.Send()
	disableRetries(rw)
	if err != nil {
		aerr := err.(awserr.Error)
		tracing.LogResponseCode(tracing.GetSpan(r), 400)
		rw.WriteHeader(400)
		rw.Write([]byte(aerr.Code() + aerr.Error()))
		return
	}

	rw.Header().Del("X-User-Context")
	rw.Header().Del("X-Request-Context")
	var objMap map[string]*json.RawMessage
	statusCode := 200

	err = json.Unmarshal(resp.Payload, &objMap)
	if err == nil {
		if val, ok := objMap["statusCode"]; ok {
			statusCode, _ = strconv.Atoi(string(*val))
		}
	} else {
		log.Errorf("Fail to parse response status code: %v", err)
	}

	tracing.LogResponseCode(tracing.GetSpan(r), statusCode)
	rw.WriteHeader(statusCode)
	rw.Write(resp.Payload)
	return
}

func findRetryRW(rw reflect.Value) (reflect.Value) {
	for _, field := range []string{"RW", "W"} {
		reflectField := reflect.Indirect(rw.Elem()).FieldByName(field)
		if reflectField.IsValid() {
			return findRetryRW(reflectField)
		}
	}
	return rw
}

func disableRetries(rw http.ResponseWriter) {
	_rw := rw
	ok := false
	var retryRW = findRetryRW(reflect.ValueOf(rw))
	if retryRW.IsValid() {
		_rw, ok = retryRW.Interface().(retryResponseWriter)
	}
	if !ok {
		_rw, ok = rw.(retryResponseWriter)
	}
	if ok {
		_rw.(retryResponseWriter).DisableRetries()
	}
}
