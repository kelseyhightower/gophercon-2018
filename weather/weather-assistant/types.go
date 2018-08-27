package function

type Weather struct {
	Event       string `json:"event"`
	Location    string `json:"location"`
	Temperature int    `json:"temperature"`
}

type WebhookResponse struct {
	FulfillmentText string `json:"fulfillmentText"`
}

type WebhookRequest struct {
	QueryResult QueryResult `json:"queryResult"`
}

type QueryResult struct {
	Action     string            `json:"action"`
	Parameters map[string]string `json:"parameters"`
}
