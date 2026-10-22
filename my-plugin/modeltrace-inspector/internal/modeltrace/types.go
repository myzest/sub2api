package modeltrace

// Answer is one model's original output for one independent challenge.
type Answer struct {
	ExpectedCount int    `json:"expected_count"`
	Text          string `json:"text"`
}

type Diagnostic struct {
	Index          int      `json:"index"`
	ParsedNumbers  int      `json:"parsed_numbers"`
	MinimumNumbers int      `json:"minimum_numbers"`
	Accepted       bool     `json:"accepted"`
	Warnings       []string `json:"warnings,omitempty"`
}

type Candidate struct {
	Model                  string  `json:"model"`
	DisplayName            string  `json:"display_name"`
	Probability            float64 `json:"probability"`
	ProfileSimilarity      float64 `json:"profile_similarity"`
	Score                  float64 `json:"score"`
	Family                 string  `json:"family"`
	FamilyName             string  `json:"family_name"`
	ConditionalProbability float64 `json:"conditional_probability"`
}

type FamilyProbability struct {
	Family      string  `json:"family"`
	DisplayName string  `json:"display_name"`
	Probability float64 `json:"probability"`
}

type Calibration struct {
	Queries    string   `json:"queries"`
	Beta       float64  `json:"beta"`
	CVAccuracy *float64 `json:"cv_accuracy"`
}

// Result matches the upstream website's score fields. Probability is a closed
// candidate-set attribution, not an intelligence score or a verified identity.
type Result struct {
	Prediction           string              `json:"prediction"`
	PredictionName       string              `json:"prediction_name"`
	Probability          float64             `json:"probability"`
	UsedOutputs          int                 `json:"used_outputs"`
	Results              []Candidate         `json:"results"`
	Diagnostics          []Diagnostic        `json:"diagnostics"`
	Calibration          Calibration         `json:"calibration"`
	FamilyPrediction     string              `json:"family_prediction"`
	FamilyPredictionName string              `json:"family_prediction_name"`
	FamilyProbability    float64             `json:"family_probability"`
	FamilyProbabilities  []FamilyProbability `json:"family_probabilities"`
	Method               string              `json:"method"`
}

type ModelInfo struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	Family        string `json:"family"`
	FamilyName    string `json:"family_name"`
	ResponseCount int    `json:"response_count"`
}

type Metadata struct {
	Revision       string      `json:"revision"`
	BuiltAt        string      `json:"built_at"`
	SHA256         string      `json:"sha256"`
	ModelCount     int         `json:"model_count"`
	ReferenceCount int         `json:"reference_count"`
	Models         []ModelInfo `json:"models"`
}
