# Images

## Logo

- `wordmark-dark.svg` and `wordmark-light.svg` are the logo for dark and light backgrounds: two linked gold rings, "chainmail" in DM Mono 500 and a block cursor. The letters are outlined, so the files need no font. The cursor blinks, and stays still for visitors who ask for reduced motion.
- `favicon.svg` is the rings on a navy rounded square. `favicon.ico`, `favicon-192.png` and `apple-touch-icon.png` are the same mark at icon sizes. The Apple icon is square, because iOS rounds the corners itself.

The repository README shows the wordmark too, through a `<picture>` element that picks the dark or light file.

## Link preview

- `social-card.png` is the 1200x630 image that link previews show: the wordmark, the headline and the install command, with the rings drawn large on the right.

The landing page keeps its own copies of these files in `site/public/img/`.
