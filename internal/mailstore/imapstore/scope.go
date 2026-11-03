package imapstore

import "context"

// SearchFolders implements mailstore.Scoper: the folders Search runs in for
// in when it has no bounds, in the same order and with the same errors.
// Like Search it reads the folder list the store keeps, listing folders
// only when it has none. ListFolders refreshes that list, so a caller that
// must see folders made elsewhere since calls ListFolders first.
func (s *Store) SearchFolders(ctx context.Context, in string) ([]string, error) {
	folders, err := s.folderList(ctx, false)
	if err != nil {
		return nil, err
	}
	if s.gmail() {
		return gmailSources(folders, in)
	}
	return standardSources(folders, in)
}
