// An image worker that dereferences a NULL pointer on a malformed input.
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

struct image { int width, height; unsigned char *pixels; };

static struct image *decode(const char *name) {
    if (strstr(name, "corrupt")) return NULL; // bug: callers do not check
    struct image *img = malloc(sizeof *img);
    img->width = 640; img->height = 480;
    img->pixels = calloc(640 * 480, 1);
    return img;
}

static long checksum(struct image *img) {
    long sum = 0;
    for (int i = 0; i < img->width * img->height; i++) sum += img->pixels[i];
    return sum;
}

int main(void) {
    const char *queue[] = {"cat.png", "dog.png", "banner.png", "corrupt-upload-8812.png"};
    int delay = getenv("DELAY_SECONDS") ? atoi(getenv("DELAY_SECONDS")) : 15;
    setvbuf(stdout, NULL, _IONBF, 0);
    printf("thumbnail-worker started\n");
    for (int i = 0; i < 4; i++) {
        sleep(delay / 4 + 1);
        printf("processing %s\n", queue[i]);
        struct image *img = decode(queue[i]);
        printf("checksum %ld\n", checksum(img));
    }
    return 0;
}
