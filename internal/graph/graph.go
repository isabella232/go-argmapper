package graph

// Graph represents a graph structure.
//
// Unless otherwise documented, it is unsafe to call any method on Graph concurrently.
type Graph struct {
	// adjacency represents graphs using an adjaency list. Vertices are
	// represented using their hash codes for simpler equaliy checks.
	adjacencyOut map[interface{}]map[interface{}]struct{}
	adjacencyIn  map[interface{}]map[interface{}]struct{}

	// hash maintains the mapping of hash codes to the representative Vertex.
	// It is assumed that two identical hashcodes of v1 and v2 are semantically
	// the same Vertex even if v1 != v2 in Go.
	hash map[interface{}]Vertex
}

// Add adds a vertex to the graph.
func (g *Graph) Add(v Vertex) Vertex {
	g.init()
	h := hashcode(v)
	if _, ok := g.adjacencyOut[h]; !ok {
		g.adjacencyOut[h] = make(map[interface{}]struct{})
		g.adjacencyIn[h] = make(map[interface{}]struct{})
		g.hash[h] = v
	}
	return v
}

// AddEdge adds a directed edge to the graph from v1 to v2. Both v1 and v2
// must already be in the Graph via Add or this will do nothing.
func (g *Graph) AddEdge(v1, v2 Vertex) {
	g.init()
	h1, h2 := hashcode(v1), hashcode(v2)

	// If we already are in the output map, then we assume we're alread in
	// the in map as well as exit.
	outMap := g.adjacencyOut[h1]
	if _, ok := outMap[h2]; ok {
		return
	}
	inMap := g.adjacencyIn[h2]

	// Add our edges
	outMap[h2] = struct{}{}
	inMap[h1] = struct{}{}
}

func (g *Graph) RemoveEdge(v1, v2 Vertex) {
	g.init()
	h1, h2 := hashcode(v1), hashcode(v2)
	delete(g.adjacencyOut[h1], h2)
	delete(g.adjacencyIn[h2], h1)
}

// Copy copies the graph. In the copy, any added or removed edges do not
// affect the original graph. The vertices themselves are not deep copied.
func (g *Graph) Copy() *Graph {
	var g2 Graph
	g2.init()

	for k, set := range g.adjacencyOut {
		copy := make(map[interface{}]struct{})
		for k, v := range set {
			copy[k] = v
		}
		g2.adjacencyOut[k] = copy
	}
	for k, set := range g.adjacencyIn {
		copy := make(map[interface{}]struct{})
		for k, v := range set {
			copy[k] = v
		}
		g2.adjacencyIn[k] = copy
	}
	for k, v := range g.hash {
		g2.hash[k] = v
	}

	return &g2
}

func (g *Graph) init() {
	if g.adjacencyOut == nil {
		g.adjacencyOut = make(map[interface{}]map[interface{}]struct{})
	}
	if g.adjacencyIn == nil {
		g.adjacencyIn = make(map[interface{}]map[interface{}]struct{})
	}
	if g.hash == nil {
		g.hash = make(map[interface{}]Vertex)
	}
}
