package argmapper

import (
	"fmt"
	"reflect"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-multierror"
	"github.com/mitchellh/go-argmapper/internal/graph"
)

func (f *Func) Call(opts ...Arg) Result {
	// buildErr accumulates any errors that we return at various checkpoints
	var buildErr error

	// Build up our args
	builder := &argBuilder{
		logger: hclog.L(),
		named:  make(map[string]reflect.Value),
	}
	for _, opt := range opts {
		if err := opt(builder); err != nil {
			buildErr = multierror.Append(buildErr, err)
		}
	}

	// If we got errors building up arguments then we're done.
	if buildErr != nil {
		return resultError(buildErr)
	}

	log := builder.logger

	// Build the graph. The first step is to add our function and all the
	// requirements of the function. We keep track of this in vertexF and
	// vertexT, respectively, because we'll need these later.
	var g graph.Graph
	vertexF := g.Add(&funcVertex{Func: f})                  // Function
	vertexT := make([]graph.Vertex, 0, len(f.input.fields)) // Targets (function requirements)
	for k, f := range f.input.fields {
		// Add our target
		target := g.Add(&valueVertex{
			Name: k,
			Type: f.Type,
		})

		// Connect the function to the target (function requires target)
		g.AddEdge(vertexF, target)

		// Track for later
		vertexT = append(vertexT, target)
	}

	// If we have converters, add those. See ConvSet.graph for more details.
	ConvSet(builder.convs).graph(&g)

	// Next, we add "inputs", which are the given named values that
	// we already know about. These are tracked as "vertexI". We also
	// create a shared "input root" tracked as "vertexIRoot". The shared
	// input root lets us have a single root entrypoint for the graph.
	vertexIRoot := g.Add(&inputRootVertex{})
	vertexI := make([]graph.Vertex, 0, len(builder.named))
	for k, v := range builder.named {
		// Add the input
		input := g.Add(&valueVertex{
			Name:  k,
			Type:  v.Type(),
			Value: v,
		})

		// Input depends on the input root
		g.AddEdge(input, vertexIRoot)

		// Track
		vertexI = append(vertexI, input)
	}

	// Next, for all values we may have or produce, we need to create
	// the vertices for the type-only value. This lets us say, for example,
	// that an input "A string" satisfies anything that requires only "string".
	for _, raw := range g.Vertices() {
		v, ok := raw.(*valueVertex)
		if !ok {
			continue
		}

		// We only add an edge from the output if we require a value.
		// If we already have a value then we don't need to request one.
		if !v.Value.IsValid() {
			g.AddEdgeWeighted(v, g.Add(&typedOutputVertex{
				Type: v.Type,
			}), weightTyped)
		}

		// We always add an edge from the arg to the value, whether it
		// has one or not. In the next step, we'll prune any typed arguments
		// that already have a satisfied value.
		g.AddEdgeWeighted(g.Add(&typedArgVertex{
			Type: v.Type,
		}), v, weightTyped)
	}

	// TODO: explain why
	for _, raw := range g.Vertices() {
		v, ok := raw.(*typedArgVertex)
		if !ok {
			continue
		}

		keep := map[interface{}]struct{}{}
		for _, out := range g.OutEdges(v) {
			if v, ok := out.(*valueVertex); ok && v.Value.IsValid() {
				keep[graph.VertexID(out)] = struct{}{}
				break
			}
		}

		if len(keep) > 0 {
			for _, v := range vertexI {
				keep[graph.VertexID(v)] = struct{}{}
			}

			for _, out := range g.OutEdges(v) {
				if _, ok := keep[graph.VertexID(out)]; !ok {
					g.RemoveEdge(v, out)
				}
			}
		}
	}

	// Next we do a DFS from each input A in I to the function F.
	// This gives us the full set of reachable nodes from our inputs
	// and at most to F. Using this information, we can prune any nodes
	// that are guaranteed to be unused.
	//
	// DFS from the input root and record what we see. We have to reverse the
	// graph here because we typically have out edges pointing to
	// requirements, but we're going from requirements (inputs) to
	// the function.
	visited := map[interface{}]struct{}{graph.VertexID(vertexF): struct{}{}}
	g.Reverse().DFS(vertexIRoot, func(v graph.Vertex, next func() error) error {
		if v == vertexF {
			return nil
		}

		visited[graph.VertexID(v)] = struct{}{}
		return next()
	})

	// Output our full graph before we do any pruning
	log.Trace("full graph", "graph", g.String())

	// Remove all the non-visited vertices. After this, what we'll have
	// is a graph that has many paths getting us from inputs to function,
	// but we will have no spurious vertices that are unreachable from our
	// inputs.
	for _, v := range g.Vertices() {
		if _, ok := visited[graph.VertexID(v)]; !ok {
			g.Remove(v)
		}
	}
	log.Trace("graph after input DFS", "graph", g.String())

	// Get the topological sort. We only need this so that we can start
	// calculating shortest path. We'll use shortest path information to
	// determine the ideal path from our inputs to the function.
	topo := g.Reverse().KahnSort()
	log.Trace("topological sort", "sort", topo)

	visited = map[interface{}]struct{}{graph.VertexID(vertexF): struct{}{}}
	paths := make([][]graph.Vertex, len(vertexT))
	for i, current := range vertexT {
		if v := g.Vertex(graph.VertexID(current)); v != nil {
			current = v
		} else {
			return Result{buildErr: fmt.Errorf(
				"argument cannot be satisfied: %s", current.(*valueVertex).Name)}
		}

		currentG := &g

		// For value vertices, we discount any other values that share the
		// same name. This lets our shortest paths prefer matching through
		// same-named arguments.
		if currentValue, ok := current.(*valueVertex); ok {
			currentG = currentG.Copy()
			for _, raw := range currentG.Vertices() {
				if v, ok := raw.(*valueVertex); ok && v.Name == currentValue.Name {
					for _, src := range currentG.InEdges(raw) {
						currentG.AddEdgeWeighted(src, raw, weightMatchingName)
					}
				}
			}
		}

		// Get the shortest path data. We need to reverse the graph here since
		// the topo sort is from the reversal as well. We have to calculate
		// the shortest path for each vertexT value because we may change
		// edge weights above. We can reuse the topo value because the shape
		// of the graph is not changing.
		_, edgeTo := currentG.Reverse().TopoShortestPath(topo)

		// With the latest shortest paths, let's add the path for this target.
		paths[i] = currentG.EdgeToPath(current, edgeTo)
		log.Trace("path for target", "target", current, "path", paths[i])

		// FIXME
		for {
			currenth := graph.VertexID(current)
			visited[currenth] = struct{}{}
			current = edgeTo[currenth]
			if current == nil {
				break
			}

			// TODO: we need to check the in-edges here for function types
			// and make sure that all the function parameters are visited
			// since that is required.
		}
	}
	for _, v := range g.Vertices() {
		if _, ok := visited[graph.VertexID(v)]; !ok {
			g.Remove(v)
		}
	}
	log.Trace("graph after shortest path detection", "graph", g.String())

	// Go through each path
	state := newCallState()
	remaining := len(paths)
	idx := 0
	for remaining > 0 {
		path := paths[idx]
		if len(path) == 0 {
			idx++
			continue
		}

		pathIdx := 0
		for pathIdx = 0; pathIdx < len(path); pathIdx++ {
			log := log.With("current", path[pathIdx])
			log.Trace("executing node")

			switch v := path[pathIdx].(type) {
			case *valueVertex:
				// Store the last viewed vertex in our path state
				state.Value = v

				if pathIdx > 0 {
					prev := path[pathIdx-1]
					if r, ok := prev.(*typedOutputVertex); ok {
						log.Trace("setting node value", "value", r.Value)
						v.Value = r.Value
					}
				}

				// If we have a valid value set, then put it on our named list.
				if v.Value.IsValid() {
					state.Named[v.Name] = v.Value
				}

			case *typedArgVertex:
				// The value of this is the last value vertex we saw. The graph
				// walk should ensure this is the correct type.
				v.Value = *state.Value

				// Setup our mapping so that we know that this wildcard
				// maps to this name.
				state.Mapping[v.Name] = &v.Value
				state.TypedValue[v.Type] = v.Value.Value

			case *typedOutputVertex:
				// Set the typed value we can read from.
				state.TypedValue[v.Type] = v.Value

			case *convVertex:
				if err := f.convExecute(log.Named(graph.VertexName(v)), &g, topo, v, state); err != nil {
					return resultError(err)
				}

				result := v.Conv.call(state)
				if err := result.Err(); err != nil {
					return resultError(err)
				}

				// Update our graph nodes
				v.Conv.outputValues(result, g.InEdges(v), state)

			default:
				panic(fmt.Sprintf("unknown vertex: %v", v))
			}
		}

		paths[idx] = path[pathIdx:]
		if len(paths[idx]) == 0 {
			remaining--
		}
		idx++
	}

	return f.call(state)
}

// convExecute executes the the given convVertex by ensuring we satisfy
// all the inbound arguments first and then calling it.
func (f *Func) convExecute(
	log hclog.Logger,
	g *graph.Graph,
	topo graph.TopoOrder,
	target *convVertex,
	state *callState,
) error {
	// Look at the out edges, since these are the requirements for the conv
	// and determine which inputs we need values for. If we have a value
	// already then we skip the target because we assume it is already in
	// the state.
	var vertexT []graph.Vertex
	for _, out := range g.OutEdges(target) {
		skip := false
		switch v := out.(type) {
		case *valueVertex:
			skip = v.Value.IsValid()

		case *typedArgVertex:
			skip = v.Value.Value.IsValid()
		}

		if !skip {
			vertexT = append(vertexT, out)
		}
	}

	paths := make([][]graph.Vertex, len(vertexT))
	for i, current := range vertexT {
		currentG := g

		// For value vertices, we discount any other values that share the
		// same name. This lets our shortest paths prefer matching through
		// same-named arguments.
		if currentValue, ok := current.(*valueVertex); ok {
			currentG = currentG.Copy()
			for _, raw := range currentG.Vertices() {
				if v, ok := raw.(*valueVertex); ok && v.Name == currentValue.Name {
					for _, src := range currentG.InEdges(raw) {
						currentG.AddEdgeWeighted(src, raw, weightMatchingName)
					}
				}
			}
		}

		// Get the shortest path data. We need to reverse the graph here since
		// the topo sort is from the reversal as well. We have to calculate
		// the shortest path for each vertexT value because we may change
		// edge weights above. We can reuse the topo value because the shape
		// of the graph is not changing.
		_, edgeTo := currentG.Reverse().TopoShortestPath(topo.Until(current))

		// With the latest shortest paths, let's add the path for this target.
		paths[i] = currentG.EdgeToPath(current, edgeTo)
		log.Trace("path for target", "target", current, "path", paths[i])
	}

	// Go through each path
	remaining := len(paths)
	idx := 0
	for remaining > 0 {
		path := paths[idx]
		if len(path) == 0 {
			idx++
			continue
		}

		pathIdx := 0
		for pathIdx = 0; pathIdx < len(path); pathIdx++ {
			log := log.With("current", path[pathIdx])
			log.Trace("executing node")

			switch v := path[pathIdx].(type) {
			case *valueVertex:
				// Store the last viewed vertex in our path state
				state.Value = v

				if pathIdx > 0 {
					prev := path[pathIdx-1]
					if r, ok := prev.(*typedOutputVertex); ok {
						log.Trace("setting node value", "value", r.Value)
						v.Value = r.Value
					}
				}

				// If we have a valid value set, then put it on our named list.
				if v.Value.IsValid() {
					state.Named[v.Name] = v.Value
				}

			case *typedArgVertex:
				// The value of this is the last value vertex we saw. The graph
				// walk should ensure this is the correct type.
				v.Value = *state.Value

				// Setup our mapping so that we know that this wildcard
				// maps to this name.
				state.Mapping[v.Name] = &v.Value
				state.TypedValue[v.Type] = v.Value.Value

			case *typedOutputVertex:
				// Set the typed value we can read from.
				state.TypedValue[v.Type] = v.Value

			case *convVertex:
				// Reach our arguments if they aren't already.
				if err := f.convExecute(
					log.Named(graph.VertexName(v)),
					g,
					topo,
					v,
					state,
				); err != nil {
					return err
				}

				// Call our function.
				result := v.Conv.call(state)
				if err := result.Err(); err != nil {
					return err
				}

				// Update our graph nodes and continue
				v.Conv.outputValues(result, g.InEdges(v), state)

			default:
				panic(fmt.Sprintf("unknown vertex: %v", v))
			}
		}

		paths[idx] = path[pathIdx:]
		if len(paths[idx]) == 0 {
			remaining--
		}
		idx++
	}

	// Reached our goal
	return nil
}

// call -- the unexported version of Call -- calls the function directly
// with the given named arguments. This skips the whole graph creation
// step by requiring args satisfy all required arguments.
func (f *Func) call(state *callState) Result {
	// Initialize the struct we'll be populating
	var buildErr error
	structVal := f.input.New()
	for k := range f.input.fields {
		v, ok := state.Named[k]
		if !ok {
			buildErr = multierror.Append(buildErr, fmt.Errorf(
				"argument cannot be satisfied: %s", k))
			continue
		}

		structVal.FieldNamed(k).Set(v)
	}

	for _, f := range f.input.typedFields {
		v, ok := state.TypedValue[f.Type]
		if !ok {
			buildErr = multierror.Append(buildErr, fmt.Errorf(
				"argument cannot be satisfied: %d", f.Index))
			continue
		}

		structVal.Field(f.Index).Set(v)
	}

	// If there was an error setting up the struct, then report that.
	if buildErr != nil {
		return Result{buildErr: buildErr}
	}

	// Call our function
	out := f.fn.Call(structVal.CallIn())
	return Result{out: out}
}

type callState struct {
	Named map[string]reflect.Value

	Result Result

	// Value is the last seen value vertex. This state is preserved so
	// we can set the typedVertex values properly.
	Value *valueVertex

	// Typed holds the last seen typedVertex (source or destination
	// since they share the same value).
	Mapping    map[string]*valueVertex
	TypedValue map[reflect.Type]reflect.Value
}

func newCallState() *callState {
	return &callState{
		Named:      map[string]reflect.Value{},
		Mapping:    map[string]*valueVertex{},
		TypedValue: map[reflect.Type]reflect.Value{},
	}
}

type pathState struct {
	*callState

	// Result is the result from the last call.
	Result Result
}