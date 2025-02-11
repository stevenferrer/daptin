package resource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/artpar/api2go"
	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/ghodss/yaml"
	"github.com/imroc/req"
	log "github.com/sirupsen/logrus"
	"regexp"
	"strings"
	"time"
)

// Mode defines a mode of operation for example generation.
type Mode int

const (
	// ModeRequest is for the request body (writes to the server)
	ModeRequest Mode = iota
	// ModeResponse is for the response body (reads from the server)
	ModeResponse
)

/**
  Integration action performer
*/
type IntegrationActionPerformer struct {
	cruds            map[string]*DbResource
	integration      Integration
	router           *openapi3.Swagger
	commandMap       map[string]*openapi3.Operation
	pathMap          map[string]string
	methodMap        map[string]string
	encryptionSecret []byte
}

// Name of the action
func (d *IntegrationActionPerformer) Name() string {
	return d.integration.Name
}

// Perform integration api
func (d *IntegrationActionPerformer) DoAction(request Outcome, inFieldMap map[string]interface{}) (api2go.Responder, []ActionResponse, []error) {

	operation, ok := d.commandMap[request.Method]
	method := d.methodMap[request.Method]
	path, ok := d.pathMap[request.Method]
	pathItem := d.router.Paths.Find(path)

	securitySchemaMap := d.router.Components.SecuritySchemes
	//selectedSecuritySchema := &openapi3.SecuritySchemeRef{}

	if !ok || pathItem == nil {
		return nil, nil, []error{errors.New("no such method")}
	}

	r := req.New()

	decryptedSpec, err := Decrypt(d.encryptionSecret, d.integration.AuthenticationSpecification)

	if err != nil {
		log.Errorf("Failed to decrypted auth spec: %v", err)
	}
	authKeys := make(map[string]interface{})
	json.Unmarshal([]byte(decryptedSpec), &authKeys)

	for key, val := range authKeys {
		inFieldMap[key] = val
	}

	url := d.router.Servers[0].URL + path

	matches, err := GetParametersNames(url)

	if err != nil {
		return nil, nil, []error{err}
	}

	for _, matc := range matches {
		value := inFieldMap[matc]
		url = strings.Replace(url, "{"+matc+"}", value.(string), -1)
	}

	evaluateString(url, inFieldMap)
	var resp *req.Resp
	arguments := make([]interface{}, 0)

	if operation.RequestBody != nil {

		requestBodyRef := operation.RequestBody.Value
		requestContent := requestBodyRef.Content

		for mediaType, spec := range requestContent {
			switch mediaType {
			case "application/json":

				requestBody, err := CreateRequestBody(ModeRequest, "", spec.Schema.Value, inFieldMap)
				if err != nil || spec == nil {
					log.Errorf("Failed to create request body for calling [%v][%v]", d.integration.Name, request.Method)
				} else {
					arguments = append(arguments, req.BodyJSON(requestBody))
				}

			case "application/x-www-form-urlencoded":
				requestBody, err := CreateRequestBody(ModeRequest, "", spec.Schema.Value, inFieldMap)
				if err != nil || spec == nil {
					log.Errorf("Failed to create request body for calling [%v][%v]", d.integration.Name, request.Method)
				} else {
					arguments = append(arguments, req.Param(requestBody.(map[string]interface{})))
				}

			}
		}
		//hasRequestBody = true
	}

	authDone := false

	if operation.Security != nil {
		secMethods := operation.Security

		*secMethods = append(*secMethods, d.router.Security...)

		for _, security := range *secMethods {

			allDone := true
			for secName := range security {
				spec := securitySchemaMap[secName]

				done := false
				switch spec.Value.Type {

				case "oauth2":

					oauthTokenId, ok := authKeys["oauth_token_id"].(string)

					if ok {
						oauthToken, oauthConfig, err := d.cruds["oauth_token"].GetTokenByTokenReferenceId(oauthTokenId)

						if err != nil {
							allDone = false
							break
						}
						tokenSource := oauthConfig.TokenSource(context.Background(), oauthToken)

						if oauthToken.Expiry.Before(time.Now()) {

							oauthToken, err = tokenSource.Token()
							if err != nil {
								allDone = false
								break
							}
							d.cruds["oauth_token"].UpdateAccessTokenByTokenReferenceId(oauthTokenId, oauthToken.Type(), oauthToken.Expiry.Unix())
						}

						arguments = append(arguments, req.Header{
							"Authorization": "Bearer " + oauthToken.AccessToken,
						})

					}

				case "http":
					switch spec.Value.Scheme {
					case "basic":
						basic := authKeys["token"].(string)
						header := base64.StdEncoding.EncodeToString([]byte(basic))

						if ok {
							arguments = append(arguments, req.Header{
								"Authorization": "Basic " + header,
							})
							done = true
						}

					case "bearer":
						token, ok := authKeys["token"].(string)

						if ok {

							arguments = append(arguments, req.Header{
								"Authorization": "Bearer " + token,
							})
							done = true
						}

					}

				case "apiKey":
					switch spec.Value.In {

					case "cookie":
						name := spec.Value.Name
						value, ok := authKeys[name].(string)
						if ok {

							arguments = append(arguments, req.Header{
								"Cookie": fmt.Sprintf("%s=%s", name, value),
							})
							done = true
						}

					case "header":
						name := spec.Value.Name
						value, ok := authKeys[name].(string)
						if ok {

							arguments = append(arguments, req.Header{
								name: value,
							})
							done = true
						}

					case "query":

						name := spec.Value.Name
						value := authKeys[name].(string)
						arguments = append(arguments, req.QueryParam{
							name: value,
						})
						done = true
					}
				}

				if !done {
					allDone = false
					break
				}

			}
			if allDone {
				authDone = true
			}

		}
	}

	if !authDone {
		switch strings.ToLower(d.integration.AuthenticationType) {

		case "oauth2":

			oauthTokenId, ok := authKeys["oauth_token_id"].(string)

			if ok {
				oauthToken, oauthConfig, err := d.cruds["oauth_token"].GetTokenByTokenReferenceId(oauthTokenId)
				if err != nil {

					tokenSource := oauthConfig.TokenSource(context.Background(), oauthToken)

					if oauthToken.Expiry.Before(time.Now()) {
						oauthToken, err = tokenSource.Token()
						d.cruds["oauth_token"].UpdateAccessTokenByTokenReferenceId(oauthTokenId, oauthToken.Type(), oauthToken.Expiry.Unix())
					}

					arguments = append(arguments, req.Header{
						"Authorization": "Bearer " + oauthToken.AccessToken,
					})
					authDone = true
				}

			}

		case "http":
			switch authKeys["scheme"].(string) {
			case "basic":
				token := authKeys["token"].(string)
				header := base64.StdEncoding.EncodeToString([]byte(token))

				if ok {
					arguments = append(arguments, req.Header{
						"Authorization": "Basic " + header,
					})
					authDone = true
				}

			case "bearer":
				token, ok := authKeys["token"].(string)

				if ok {

					arguments = append(arguments, req.Header{
						"Authorization": "Bearer " + token,
					})
					authDone = true
				}

			}
		case "apiKey":
			switch authKeys["in"] {

			case "cookie":
				name := authKeys["name"].(string)
				value, ok := authKeys[name].(string)
				if ok {

					arguments = append(arguments, req.Header{
						"Cookie": fmt.Sprintf("%s=%s", name, value),
					})
					authDone = true
				}

			case "header":
				name := authKeys["name"].(string)
				value := authKeys[name].(string)
				arguments = append(arguments, req.Header{
					name: value,
				})
				authDone = true

			case "query":

				name := authKeys["name"].(string)
				value := authKeys[name].(string)
				arguments = append(arguments, req.QueryParam{
					name: value,
				})
				authDone = true
			}

		}
	}

	parameters := operation.Parameters
	for _, param := range parameters {

		if param.Value.In == "path" {
			continue
		}

		if param.Value.In == "body" {
			continue
		}
		if param.Value.In == "header" {
			parameterValues := make(map[string]string)
			value, err := CreateRequestBody(ModeRequest, param.Value.Name, param.Value.Schema.Value, inFieldMap)
			if err != nil {
				log.Errorf("Failed to create parameters for calling [%v][%v]", d.integration.Name, request.Method)
				return nil, nil, []error{err}
			}
			parameterValues[param.Value.Name] = value.(string)
			arguments = append(arguments, req.Header(parameterValues))

		}

		if param.Value.In == "query" {
			parameterValues := make(map[string]interface{})
			value, err := CreateRequestBody(ModeRequest, param.Value.Name, param.Value.Schema.Value, inFieldMap)
			if err != nil {
				log.Errorf("Failed to create parameters for calling [%v][%v]", d.integration.Name, request.Method)
				return nil, nil, []error{err}
			}
			parameterValues[param.Value.Name] = value
			arguments = append(arguments, req.QueryParam(parameterValues))
		}

	}

	switch strings.ToLower(method) {
	case "post":
		resp, err = r.Post(url, arguments...)

	case "get":
		resp, err = r.Get(url, arguments...)
	case "delete":
		resp, err = r.Delete(url, arguments...)
	case "patch":
		resp, err = r.Patch(url, arguments...)
	case "put":
		resp, err = r.Put(url, arguments...)
	case "options":
		resp, err = r.Options(url, arguments...)

	}

	var res map[string]interface{}
	resp.ToJSON(&res)
	responder := NewResponse(nil, res, resp.Response().StatusCode, nil)
	return responder, []ActionResponse{}, nil
}
func GetParametersNames(s string) ([]string, error) {
	ret := make([]string, 0)
	templateVar, err := regexp.Compile(`\{([^}]+)\}`)
	if err != nil {
		return ret, err
	}

	matches := templateVar.FindAllStringSubmatch(s, -1)

	for _, match := range matches {
		ret = append(ret, match[1])
	}
	return ret, nil
}

// OpenAPIExample creates an example structure from an OpenAPI 3 schema
// object, which is an extended subset of JSON Schema.
// https://github.com/OAI/OpenAPI-Specification/blob/master/versions/3.0.1.md#schemaObject
func CreateRequestBody(mode Mode, name string, schema *openapi3.Schema, values map[string]interface{}) (interface{}, error) {

	switch {
	case schema.Type == "boolean":
		value, ok := values[name]

		if !ok {
			return false, nil
		}

		valString, ok := value.(string)

		if ok {
			if strings.ToLower(valString) == "true" {
				return true, nil
			}
		}
		valBool, ok := value.(bool)

		if ok {
			return valBool, nil
		}

		return false, nil
	case schema.Type == "number", schema.Type == "integer":

		value, ok := values[name].(float64)

		if !ok {
			valueInt, ok := values[name].(int64)
			if ok {
				value = float64(valueInt)
			}
		}

		if schema.Type == "integer" {
			return int(value), nil
		}

		return value, nil
	case schema.Type == "string":
		str := values[name]
		if str == nil {
			return nil, nil
		}

		example := str.(string)
		return example, nil
	case schema.Type == "array", schema.Items != nil:

		val := values[name]

		if val == nil {
			val = []map[string]interface{}{values}
		}

		var ok bool
		var mapVal []map[string]interface{}
		mapVal, ok = val.([]map[string]interface{})
		if !ok {
			arrayVal, ok := val.([]interface{})
			if !ok {
				return []interface{}{}, errors.New(fmt.Sprintf("type not array type [%v]: %v", name, val))
			}
			mapVal = make([]map[string]interface{}, 0)

			for _, row := range arrayVal {
				mapVal = append(mapVal, row.(map[string]interface{}))
			}
		}

		var items []interface{}

		if schema.Items != nil && schema.Items.Value != nil {

			for _, item := range mapVal {

				ex, err := CreateRequestBody(mode, name, schema.Items.Value, item)

				if err != nil {
					return nil, errors.New(fmt.Sprintf("failed to convert item to body: [%v][%v] == %v", name, item, err))
				}

				items = append(items, ex)
			}

		}

		return items, nil
	case schema.Type == "object", len(schema.Properties) > 0:
		example := map[string]interface{}{}

		for k, v := range schema.Properties {
			if excludeFromMode(mode, v.Value) {
				continue
			}

			ex, err := CreateRequestBody(mode, k, v.Value, values)
			if err != nil {
				return nil, fmt.Errorf("can't get example for '%s'", k)
			}

			example[k] = ex
		}

		if schema.AdditionalProperties != nil && schema.AdditionalProperties.Value != nil {
			addl := schema.AdditionalProperties.Value

			if !excludeFromMode(mode, addl) {
				ex, err := CreateRequestBody(mode, name, addl, values)
				if err != nil {
					return nil, fmt.Errorf("can't get example for additional properties")
				}
				example["additionalPropertyName"] = ex
			}
		}

		return example, nil
	}

	return nil, errors.New("not a valid schema")
}

// excludeFromMode will exclude a schema if the mode is request and the schema
// is read-only, or if the mode is response and the schema is write only.
func excludeFromMode(mode Mode, schema *openapi3.Schema) bool {
	if schema == nil {
		return true
	}

	if mode == ModeRequest && schema.ReadOnly {
		return true
	} else if mode == ModeResponse && schema.WriteOnly {
		return true
	}

	return false
}

// Create a new action performer for becoming administrator action
func NewIntegrationActionPerformer(integration Integration, initConfig *CmsConfig, cruds map[string]*DbResource, configStore *ConfigStore) (ActionPerformerInterface, error) {

	var err error
	jsonBytes := []byte(integration.Specification)

	if integration.SpecificationFormat == "yaml" {

		jsonBytes, err = yaml.YAMLToJSON(jsonBytes)

		if err != nil {
			log.Errorf("Failed to convert yaml to json for integration: %v", err)
			return nil, err
		}

	}

	var router *openapi3.Swagger

	if integration.SpecificationLanguage == "openapiv2" {

		openapiv2Spec := openapi2.Swagger{}

		err := json.Unmarshal(jsonBytes, &openapiv2Spec)

		if err != nil {
			log.Errorf("Failed to unmarshal as openapiv2: %v", err)
			return nil, err
		}

		router, err = openapi2conv.ToV3Swagger(&openapiv2Spec)

		if err != nil {
			log.Errorf("Failed to convert to openapi v3 spec: %v", err)
			return nil, err
		}

	}

	if router == nil {

		router, err = openapi3.NewSwaggerLoader().LoadSwaggerFromData(jsonBytes)
	}

	if err != nil {
		log.Errorf("Failed to load swagger spec: %v", err)
		return nil, err
	}

	commandMap := make(map[string]*openapi3.Operation)
	pathMap := make(map[string]string)
	methodMap := make(map[string]string)
	for path, pathItem := range router.Paths {
		for method, command := range pathItem.Operations() {
			log.Printf("Register action [%v] at [%v]", command.OperationID, integration.Name)
			commandMap[command.OperationID] = command
			pathMap[command.OperationID] = path
			methodMap[command.OperationID] = method
		}
	}

	encryptionSecret, err := configStore.GetConfigValueFor("encryption.secret", "backend")
	if err != nil {
		log.Errorf("Failed to get encryption secret from config store: %v", err)
	}

	handler := IntegrationActionPerformer{
		cruds:            cruds,
		integration:      integration,
		router:           router,
		commandMap:       commandMap,
		pathMap:          pathMap,
		methodMap:        methodMap,
		encryptionSecret: []byte(encryptionSecret),
	}

	return &handler, nil

}
