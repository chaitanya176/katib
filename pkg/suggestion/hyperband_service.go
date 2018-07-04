package suggestion

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/kubeflow/katib/pkg/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Evals struct {
	id    string
	value float64
}
type Bracket []Evals

func (b Bracket) Len() int {
	return len(b)
}

func (b Bracket) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

func (b Bracket) Less(i, j int) bool {
	return b[i].value > b[j].value
}

type HyperBandParameters struct {
	eta                float64
	sMax               int
	b_l                float64
	r_l                float64
	r                  float64
	n                  int
	shloopitr          int
	currentS           int
	ResourceName       string
	ObjectiveValueName string
	evaluatingTrials   []string
}

type HyperBandSuggestService struct {
	RandomSuggestService
}

func NewHyperBandSuggestService() *HyperBandSuggestService {
	return &HyperBandSuggestService{}
}

func (h *HyperBandSuggestService) makeBracket(ctx context.Context, c api.ManagerClient, studyId string, n int, r float64, hbparam *HyperBandParameters) ([]string, []*api.Trial, error) {
	if len(hbparam.evaluatingTrials) == 0 || hbparam.shloopitr == 0 {
		return h.makeMasterBracket(ctx, c, studyId, n, r, hbparam)
	} else {
		err, b := h.evalWorkers(ctx, c, studyId, hbparam)
		if err != nil {
			return nil, nil, err
		}
		if b == nil {
			return nil, nil, nil
		}
		return h.makeChildBracket(ctx, c, b, studyId, n, r, hbparam)
	}
}

func (h *HyperBandSuggestService) makeMasterBracket(ctx context.Context, c api.ManagerClient, studyId string, n int, r float64, hbparam *HyperBandParameters) ([]string, []*api.Trial, error) {
	log.Printf("Make MasterBracket %v Trials", n)
	gsreq := &api.GetStudyRequest{
		StudyId: studyId,
	}
	gsrep, err := c.GetStudy(ctx, gsreq)
	if err != nil {
		log.Printf("GetStudy Error")
		return nil, nil, err
	}
	sconf := gsrep.StudyConfig
	tids := make([]string, n)
	ts := make([]*api.Trial, n)
	for i := 0; i < n; i++ {
		t := &api.Trial{
			StudyId: studyId,
		}
		t.ParameterSet = make([]*api.Parameter, len(sconf.ParameterConfigs.Configs))
		for j, pc := range sconf.ParameterConfigs.Configs {
			t.ParameterSet[j] = &api.Parameter{Name: pc.Name}
			t.ParameterSet[j].ParameterType = pc.ParameterType
			if pc.Name == hbparam.ResourceName {
				if pc.ParameterType == api.ParameterType_INT {
					t.ParameterSet[j].Value = strconv.Itoa(int(r))
				} else {
					t.ParameterSet[j].Value = strconv.FormatFloat(r, 'f', 4, 64)
				}
			} else {
				switch pc.ParameterType {
				case api.ParameterType_INT:
					imin, _ := strconv.Atoi(pc.Feasible.Min)
					imax, _ := strconv.Atoi(pc.Feasible.Max)
					t.ParameterSet[j].Value = strconv.Itoa(h.IntRandom(imin, imax))
				case api.ParameterType_DOUBLE:
					dmin, _ := strconv.ParseFloat(pc.Feasible.Min, 64)
					dmax, _ := strconv.ParseFloat(pc.Feasible.Max, 64)
					t.ParameterSet[j].Value = strconv.FormatFloat(h.DoubelRandom(dmin, dmax), 'f', 4, 64)
				case api.ParameterType_CATEGORICAL:
					t.ParameterSet[j].Value = pc.Feasible.List[h.IntRandom(0, len(pc.Feasible.List)-1)]
				}
			}
		}
		req := &api.CreateTrialRequest{
			Trial: t,
		}
		ret, err := c.CreateTrial(ctx, req)
		if err != nil {
			log.Printf("CreateTrial Error")
			return nil, nil, err
		}
		tids[i] = ret.TrialId
		t.TrialId = ret.TrialId
		ts[i] = t
	}
	return tids, ts, nil
}

func (h *HyperBandSuggestService) makeChildBracket(ctx context.Context, c api.ManagerClient, parent Bracket, studyId string, n int, r_i float64, hbparam *HyperBandParameters) ([]string, []*api.Trial, error) {
	gsreq := &api.GetStudyRequest{
		StudyId: studyId,
	}
	gsrep, err := c.GetStudy(ctx, gsreq)
	if err != nil {
		log.Printf("GetStudy Error")
		return nil, nil, err
	}
	sconf := gsrep.StudyConfig
	child := Bracket{}

	if sconf.OptimizationType == api.OptimizationType_MINIMIZE {
		child = parent[n:]
	} else if sconf.OptimizationType == api.OptimizationType_MAXIMIZE {
		child = parent[:n]
	}
	gtreq := &api.GetTrialsRequest{
		StudyId: studyId,
	}
	gtrep, err := c.GetTrials(ctx, gtreq)
	if err != nil {
		log.Printf("GetTrials Error")
		return nil, nil, err
	}
	tids := make([]string, n)
	ts := make([]*api.Trial, n)
	var rtype api.ParameterType
	for _, pc := range sconf.ParameterConfigs.Configs {
		if pc.Name == hbparam.ResourceName {
			rtype = pc.ParameterType
		}
	}
	for i, tid := range child {
		t := &api.Trial{
			StudyId: studyId,
		}
		for _, pt := range gtrep.Trials {
			if pt.TrialId == tid.id {
				t.ParameterSet = pt.ParameterSet
			}
		}
		for i, p := range t.ParameterSet {
			if p.Name == hbparam.ResourceName {
				if rtype == api.ParameterType_INT {
					t.ParameterSet[i].Value = strconv.Itoa(int(r_i))
				} else {
					t.ParameterSet[i].Value = strconv.FormatFloat(r_i, 'f', 4, 64)
				}
			}
		}
		req := &api.CreateTrialRequest{
			Trial: t,
		}
		ret, err := c.CreateTrial(ctx, req)
		if err != nil {
			log.Printf("CreateTrial Error")
			return nil, nil, err
		}
		tids[i] = ret.TrialId
		t.TrialId = ret.TrialId
		ts[i] = t
	}
	return tids, ts, nil
}

func (h *HyperBandSuggestService) purseSuggestionParameters(ctx context.Context, c api.ManagerClient, studyId string, sparam []*api.SuggestionParameter) (*HyperBandParameters, error) {
	p := &HyperBandParameters{
		eta:                -1,
		sMax:               -1,
		b_l:                -1,
		r_l:                -1,
		r:                  -1,
		n:                  -1,
		shloopitr:          -1,
		currentS:           -1,
		ResourceName:       "",
		ObjectiveValueName: "",
		evaluatingTrials:   []string{},
	}
	for _, sp := range sparam {
		switch sp.Name {
		case "eta":
			p.eta, _ = strconv.ParseFloat(sp.Value, 64)
		case "r_l":
			p.r_l, _ = strconv.ParseFloat(sp.Value, 64)
		case "ResourceName":
			p.ResourceName = sp.Value
		case "ObjectiveValueName":
			p.ObjectiveValueName = sp.Value
		case "b_l":
			p.b_l, _ = strconv.ParseFloat(sp.Value, 64)
		case "sMax":
			p.sMax, _ = strconv.Atoi(sp.Value)
		case "r":
			p.r, _ = strconv.ParseFloat(sp.Value, 64)
		case "n":
			p.n, _ = strconv.Atoi(sp.Value)
		case "shloopitr":
			p.shloopitr, _ = strconv.Atoi(sp.Value)
		case "currentS":
			p.currentS, _ = strconv.Atoi(sp.Value)
		case "evaluatingTrials":
			p.evaluatingTrials = strings.Split(sp.Value, ",")
		default:
			log.Printf("Unknown Suggestion Parameter %v", sp.Name)
		}
	}
	if p.r_l <= 0 || p.ResourceName == "" {
		log.Printf("Failed to purse Suggestion Parameter. r_l and ResourceName must be set.")
		return nil, fmt.Errorf("Suggestion Parameter set Error")
	}
	if p.eta <= 0 {
		p.eta = 3
	}
	if p.ObjectiveValueName == "" {
		gsreq := &api.GetStudyRequest{
			StudyId: studyId,
		}
		gsrep, err := c.GetStudy(ctx, gsreq)
		if err != nil {
			log.Printf("GetStudy Error")
			return nil, err
		}
		p.ObjectiveValueName = gsrep.StudyConfig.ObjectiveValueName
	}
	if p.sMax == -1 {
		p.sMax = int(math.Trunc(math.Log(p.r_l) / math.Log(p.eta)))
	}
	if p.b_l == -1 {
		p.b_l = float64((p.sMax + 1.0)) * p.r_l
	}
	if p.n == -1 {
		p.n = int(math.Ceil((p.b_l / p.r_l) * (math.Pow(p.eta, float64(p.sMax)) / float64(p.sMax+1))))
	}
	if p.currentS == -1 {
		p.currentS = p.sMax
	}
	if p.shloopitr == -1 {
		p.shloopitr = 0
	}
	if p.r == -1 {
		p.r = p.r_l * math.Pow(p.eta, float64(-p.sMax))
	}
	log.Printf("Hyb Param sMax %v", p.sMax)
	log.Printf("Hyb Param B %v", p.b_l)
	log.Printf("Hyb Param n %v", p.n)
	log.Printf("Hyb Param currentS %v", p.currentS)
	log.Printf("Hyb Param r %v", p.r)
	log.Printf("Hyb Param evaluatingTrials %v", p.evaluatingTrials)
	return p, nil
}

func (h *HyperBandSuggestService) saveSuggestionParameters(ctx context.Context, c api.ManagerClient, studyId string, algorithm string, paramId string, hbparam *HyperBandParameters) error {
	req := &api.SetSuggestionParametersRequest{
		StudyId:             studyId,
		SuggestionAlgorithm: algorithm,
		ParamId:             paramId,
	}
	sp := []*api.SuggestionParameter{}
	sp = append(sp, &api.SuggestionParameter{
		Name:  "eta",
		Value: strconv.FormatFloat(hbparam.eta, 'f', 4, 64),
	})
	sp = append(sp, &api.SuggestionParameter{
		Name:  "sMax",
		Value: strconv.Itoa(hbparam.sMax),
	})
	sp = append(sp, &api.SuggestionParameter{
		Name:  "b_l",
		Value: strconv.FormatFloat(hbparam.b_l, 'f', 4, 64),
	})
	sp = append(sp, &api.SuggestionParameter{
		Name:  "r_l",
		Value: strconv.FormatFloat(hbparam.r_l, 'f', 4, 64),
	})
	sp = append(sp, &api.SuggestionParameter{
		Name:  "r",
		Value: strconv.FormatFloat(hbparam.r, 'f', 4, 64),
	})
	sp = append(sp, &api.SuggestionParameter{
		Name:  "shloopitr",
		Value: strconv.Itoa(hbparam.shloopitr),
	})
	sp = append(sp, &api.SuggestionParameter{
		Name:  "n",
		Value: strconv.Itoa(hbparam.n),
	})
	sp = append(sp, &api.SuggestionParameter{
		Name:  "currentS",
		Value: strconv.Itoa(hbparam.currentS),
	})
	sp = append(sp, &api.SuggestionParameter{
		Name:  "ResourceName",
		Value: hbparam.ResourceName,
	})
	sp = append(sp, &api.SuggestionParameter{
		Name:  "evaluatingTrials",
		Value: strings.Join(hbparam.evaluatingTrials, ","),
	})
	req.SuggestionParameters = sp
	_, err := c.SetSuggestionParameters(ctx, req)
	return err
}

func (h *HyperBandSuggestService) evalWorkers(ctx context.Context, c api.ManagerClient, studyId string, hbparam *HyperBandParameters) (error, Bracket) {
	bracket := Bracket{}
	for _, tid := range hbparam.evaluatingTrials {
		gwreq := &api.GetWorkersRequest{
			StudyId: studyId,
			TrialId: tid,
		}
		gwrep, err := c.GetWorkers(ctx, gwreq)
		if err != nil {
			log.Printf("GetWorkers error %v", err)
			return err, nil
		}
		wl := make([]string, len(gwrep.Workers))
		for i, w := range gwrep.Workers {
			wl[i] = w.WorkerId
		}
		gmreq := &api.GetMetricsRequest{
			StudyId:      studyId,
			WorkerIds:    wl,
			MetricsNames: []string{hbparam.ObjectiveValueName},
		}
		gmrep, err := c.GetMetrics(ctx, gmreq)
		if err != nil {
			log.Printf("GetMetrics error %v", err)
			return err, nil
		}
		vs := 0.0
		for _, ml := range gmrep.MetricsLogSets {
			if ml.WorkerStatus != api.State_COMPLETED {
				return nil, nil
			}
			v, _ := strconv.ParseFloat(ml.MetricsLogs[0].Values[len(ml.MetricsLogs[0].Values)-1], 64)
			vs += v
		}
		if len(gwrep.Workers) > 0 {
			bracket = append(bracket, Evals{
				id:    gwrep.Workers[0].TrialId,
				value: vs / float64(len(gwrep.Workers)),
			})
		} else {
			return nil, nil
		}

	}
	sort.Sort(bracket)
	return nil, bracket
}

func (h *HyperBandSuggestService) hbLoopParamUpdate(studyId string, hbparam *HyperBandParameters) {
	log.Printf("HB loop s = %v", hbparam.currentS)
	hbparam.shloopitr = 0
	hbparam.n = int(math.Trunc((hbparam.b_l / hbparam.r_l) * (math.Pow(hbparam.eta, float64(hbparam.currentS)) / float64(hbparam.currentS+1))))
	hbparam.r = hbparam.r_l * math.Pow(hbparam.eta, float64(-hbparam.currentS))
}

func (h *HyperBandSuggestService) getLoopParam(studyId string, hbparam *HyperBandParameters) (int, float64) {
	log.Printf("SH loop i = %v", hbparam.shloopitr)
	n_i := int(math.Trunc(float64(hbparam.n) * math.Pow(hbparam.eta, float64(-hbparam.shloopitr))))
	r_i := hbparam.r * math.Pow(hbparam.eta, float64(hbparam.shloopitr))
	return n_i, r_i
}
func (h *HyperBandSuggestService) shLoopParamUpdate(studyId string, hbparam *HyperBandParameters) {
	hbparam.shloopitr++
	if hbparam.shloopitr > hbparam.currentS {
		hbparam.currentS--
	}
}

func (h *HyperBandSuggestService) GetSuggestions(ctx context.Context, in *api.GetSuggestionsRequest) (*api.GetSuggestionsReply, error) {
	conn, err := grpc.Dial(manager, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("could not connect: %v", err)
		return &api.GetSuggestionsReply{}, err
	}
	defer conn.Close()
	c := api.NewManagerClient(conn)
	spreq := &api.GetSuggestionParametersRequest{
		ParamId: in.ParamId,
	}
	spr, err := c.GetSuggestionParameters(ctx, spreq)
	if err != nil {
		log.Fatalf("GetParameter failed: %v", err)
		return &api.GetSuggestionsReply{}, err
	}
	hbparam, err := h.purseSuggestionParameters(ctx, c, in.StudyId, spr.SuggestionParameters)
	if err != nil {
		return &api.GetSuggestionsReply{}, err
	}

	if hbparam.currentS <= 0 {
		return &api.GetSuggestionsReply{}, nil
	}

	if hbparam.shloopitr > hbparam.currentS {
		h.hbLoopParamUpdate(in.StudyId, hbparam)
	}
	n_i, r_i := h.getLoopParam(in.StudyId, hbparam)
	tids, ts, err := h.makeBracket(ctx, c, in.StudyId, n_i, r_i, hbparam)
	if err != nil {
		return &api.GetSuggestionsReply{}, err
	}
	if tids == nil {
		return &api.GetSuggestionsReply{}, status.Errorf(codes.FailedPrecondition, "Previous workers are not completed.")
	}
	hbparam.evaluatingTrials = tids
	h.shLoopParamUpdate(in.StudyId, hbparam)
	err = h.saveSuggestionParameters(ctx, c, in.StudyId, in.SuggestionAlgorithm, in.ParamId, hbparam)
	return &api.GetSuggestionsReply{
		Trials: ts,
	}, nil
}
