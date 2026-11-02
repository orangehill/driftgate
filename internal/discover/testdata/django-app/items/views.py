import functools


def logged(f):
    @functools.wraps(f)
    def wrapper(*args, **kwargs):
        return f(*args, **kwargs)
    return wrapper


@logged
def health(request):
    return None


class ItemView:
    def get(self, request):
        return None


def item_view():
    def view(request):
        return None
    view.view_class = ItemView
    return view
