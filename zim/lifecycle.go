package zim

import "sync"

// Ciclo de vida y concurrencia (§23). Caso que pasa la primera semana: el admin
// desregistra un ZIM desde el Panel mientras alguien lo está leyendo. Close() no
// puede tirar los buffers que un lector tiene en la mano.
//
// Estados: open → closing → closed. Se adquiere un "lector" por cada Open() de una
// entrada; Close() marca closing, espera a que se drenen los lectores y pasa a
// closed. Aperturas nuevas en closing/closed → ErrClosed.
type lifeState int32

const (
	stOpen lifeState = iota
	stClosing
	stClosed
)

type lifecycle struct {
	mu      sync.Mutex
	cond    *sync.Cond
	state   lifeState
	readers int
}

func newLifecycle() *lifecycle {
	l := &lifecycle{state: stOpen}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// acquire: registra un lector activo. Falla si el archivo ya no está abierto.
func (l *lifecycle) acquire() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != stOpen {
		return ErrClosed
	}
	l.readers++
	return nil
}

// release: un lector terminó. Despierta a Close() si era el último.
func (l *lifecycle) release() {
	l.mu.Lock()
	if l.readers > 0 {
		l.readers--
	}
	last := l.readers == 0
	l.mu.Unlock()
	if last {
		l.cond.Broadcast()
	}
}

// close: marca closing, ESPERA a que se drenen los lectores y pasa a closed.
// Idempotente. La liberación real de recursos (fichero, caché del UUID) la hace el
// llamante tras esto.
func (l *lifecycle) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == stClosed {
		return
	}
	l.state = stClosing
	for l.readers > 0 {
		l.cond.Wait()
	}
	l.state = stClosed
}
