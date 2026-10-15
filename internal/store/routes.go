package store

import "context"

// Bindings and BindingByChat are deployment-scoped: a chat supplied by an
// incoming event never chooses a different host's native thread.
func (s *Store) Bindings(ctx context.Context, host string) ([]Binding, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+bindingColumns+" FROM bindings WHERE host_id=? ORDER BY thread_id", host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Binding
	for rows.Next() {
		b := Binding{HostID: host}
		if err := scanBinding(rows, &b); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

func (s *Store) BindingByChat(ctx context.Context, host, chat string) (Binding, error) {
	b := Binding{HostID: host, ChatID: chat}
	err := scanBinding(s.DB.QueryRowContext(ctx, "SELECT "+bindingColumns+" FROM bindings WHERE host_id=? AND chat_id=? AND follow_mode='group'", host, chat), &b)
	return b, err
}
