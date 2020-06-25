package datum

import (
	"archive/tar"
	"encoding/json"
	"io"
	"path"

	"github.com/gogo/protobuf/jsonpb"
	glob "github.com/pachyderm/ohmyglob"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/worker/common"
)

type IteratorV2 interface {
	Iterate(func([]*common.InputV2) error) error
}

type pfsIteratorV2 struct {
	pachClient *client.APIClient
	input      *pps.PFSInput
}

func newPFSIteratorV2(pachClient *client.APIClient, input *pps.PFSInput) IteratorV2 {
	if input.Commit == "" {
		// this can happen if a pipeline with multiple inputs has been triggered
		// before all commits have inputs
		return &pfsIteratorV2{}
	}
	return &pfsIteratorV2{
		pachClient: pachClient,
		input:      input,
	}
}

func (pi *pfsIteratorV2) Iterate(cb func([]*common.InputV2) error) error {
	if pi.input == nil {
		return nil
	}
	commit := client.NewCommit(pi.input.Repo, pi.input.Commit)
	pattern := pi.input.Glob
	return pi.pachClient.GlobFileV2(commit, pattern, func(fi *pfs.FileInfoV2) error {
		g := glob.MustCompile(pi.input.Glob, '/')
		joinOn := g.Replace(fi.File.Path, pi.input.JoinOn)
		return cb([]*common.InputV2{
			&common.InputV2{
				FileInfo:   fi,
				JoinOn:     joinOn,
				Name:       pi.input.Name,
				Lazy:       pi.input.Lazy,
				Branch:     pi.input.Branch,
				EmptyFiles: pi.input.EmptyFiles,
				S3:         pi.input.S3,
			},
		})
	})
}

type unionIteratorV2 struct {
	iterators []IteratorV2
}

func newUnionIteratorV2(pachClient *client.APIClient, inputs []*pps.Input) (IteratorV2, error) {
	ui := &unionIteratorV2{}
	for _, input := range inputs {
		di, err := NewIteratorV2(pachClient, input)
		if err != nil {
			return nil, err
		}
		ui.iterators = append(ui.iterators, di)
	}
	return ui, nil
}

// TODO: Might want to merge the iterators to keep lexicographical sorting.
func (ui *unionIteratorV2) Iterate(cb func([]*common.InputV2) error) error {
	for _, di := range ui.iterators {
		if err := di.Iterate(cb); err != nil {
			return err
		}
	}
	return nil
}

type crossIteratorV2 struct {
	iterators []IteratorV2
}

func newCrossIteratorV2(pachClient *client.APIClient, inputs []*pps.Input) (IteratorV2, error) {
	ci := &crossIteratorV2{}
	for _, input := range inputs {
		di, err := NewIteratorV2(pachClient, input)
		if err != nil {
			return nil, err
		}
		ci.iterators = append(ci.iterators, di)
	}
	return ci, nil
}

func (ci *crossIteratorV2) Iterate(cb func([]*common.InputV2) error) error {
	return iterate(nil, ci.iterators, cb)
}

func iterate(crossInputs []*common.InputV2, iterators []IteratorV2, cb func([]*common.InputV2) error) error {
	if len(iterators) == 0 {
		return cb(crossInputs)
	}
	// TODO: Might want to exit fast for the zero datums case.
	return iterators[0].Iterate(func(inputs []*common.InputV2) error {
		return iterate(append(crossInputs, inputs...), iterators[1:], cb)
	})
}

// TODO: Need inspect file.
//type gitIteratorV2 struct {
//	inputs []*common.InputV2
//}
//
//func newGitIteratorV2(pachClient *client.APIClient, input *pps.GitInput) (IteratorV2, error) {
//	if input.Commit == "" {
//		// this can happen if a pipeline with multiple inputs has been triggered
//		// before all commits have inputs
//		return &gitIteratorV2{}, nil
//	}
//	fi, err := pachClient.InspectFileV2(input.Name, input.Commit, "/commit.json")
//	if err != nil {
//		return nil, err
//	}
//	return &gitIteratorV2{
//		inputs: []*common.InputV2{
//			&common.InputV2{
//				FileInfo: fi,
//				Name:     input.Name,
//				Branch:   input.Branch,
//				GitURL:   input.URL,
//			},
//		},
//	}, nil
//}
//
//func (gi *gitIteratorV2) Iterate(cb func([]*common.InputV2) error) error {
//	if len(gi.inputs) == 0 {
//		return nil
//	}
//	return cb(gi.inputs)
//}

func newCronIteratorV2(pachClient *client.APIClient, input *pps.CronInput) IteratorV2 {
	return newPFSIteratorV2(pachClient, &pps.PFSInput{
		Name:   input.Name,
		Repo:   input.Repo,
		Branch: "master",
		Commit: input.Commit,
		Glob:   "/*",
	})
}

type fileSetIterator struct {
	pachClient   *client.APIClient
	repo, commit string
}

func NewFileSetIterator(pachClient *client.APIClient, repo, commit string, baseFileSet ...string) IteratorV2 {
	return &fileSetIterator{
		pachClient: pachClient,
		repo:       repo,
		commit:     commit,
	}
}

func (fsi *fileSetIterator) Iterate(cb func([]*common.InputV2) error) error {
	r, err := fsi.pachClient.GetTarV2(fsi.repo, fsi.commit, path.Join("/*", InputFileName))
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		_, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		decoder := json.NewDecoder(tr)
		var inputs []*common.InputV2
		for {
			input := &common.InputV2{}
			if err := jsonpb.UnmarshalNext(decoder, input); err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			inputs = append(inputs, input)
		}
		return cb(inputs)
	}
}

func NewIteratorV2(pachClient *client.APIClient, input *pps.Input) (IteratorV2, error) {
	switch {
	case input.Pfs != nil:
		return newPFSIteratorV2(pachClient, input.Pfs), nil
	case input.Union != nil:
		return newUnionIteratorV2(pachClient, input.Union)
	case input.Cross != nil:
		return newCrossIteratorV2(pachClient, input.Cross)
	//case input.Join != nil:
	//	return newJoinIterator(pachClient, input.Join)
	case input.Cron != nil:
		return newCronIteratorV2(pachClient, input.Cron), nil
		//case input.Git != nil:
		//	return newGitIteratorV2(pachClient, input.Git)
	}
	return nil, errors.Errorf("unrecognized input type: %v", input)
}

// TODO: Implement joins.
//type joinIterator struct {
//	datums   [][]*common.Input
//	location int
//}
//
//func newJoinIterator(pachClient *client.APIClient, join []*pps.Input) (Iterator, error) {
//	result := &joinIterator{}
//	om := ordered_map.NewOrderedMap()
//
//	for i, input := range join {
//		datumIterator, err := NewIterator(pachClient, input)
//		if err != nil {
//			return nil, err
//		}
//		for datumIterator.Next() {
//			x := datumIterator.Datum()
//			for _, k := range x {
//				tupleI, ok := om.Get(k.JoinOn)
//				var tuple [][]*common.Input
//				if !ok {
//					tuple = make([][]*common.Input, len(join))
//				} else {
//					tuple = tupleI.([][]*common.Input)
//				}
//				tuple[i] = append(tuple[i], k)
//				om.Set(k.JoinOn, tuple)
//			}
//		}
//	}
//
//	iter := om.IterFunc()
//	for kv, ok := iter(); ok; kv, ok = iter() {
//		tuple := kv.Value.([][]*common.Input)
//		cross, err := newCrossListIterator(pachClient, tuple)
//		if err != nil {
//			return nil, err
//		}
//		for cross.Next() {
//			result.datums = append(result.datums, cross.Datum())
//		}
//	}
//	result.location = -1
//	return result, nil
//}
//
//func (d *joinIterator) Reset() {
//	d.location = -1
//}
//
//func (d *joinIterator) Len() int {
//	return len(d.datums)
//}
//
//func (d *joinIterator) Next() bool {
//	if d.location < len(d.datums) {
//		d.location++
//	}
//	return d.location < len(d.datums)
//}
//
//func (d *joinIterator) Datum() []*common.Input {
//	var result []*common.Input
//	result = append(result, d.datums[d.location]...)
//	sortInputs(result)
//	return result
//}
//
//func (d *joinIterator) DatumN(n int) []*common.Input {
//	d.location = n
//	return d.Datum()
//}