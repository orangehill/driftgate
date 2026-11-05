<?php
// A stand-in for Laravel, just enough for laravel_rules.php: a router with
// routes to controller methods, an invokable controller and closures, one
// of them with a form request.
require __DIR__ . '/illuminate.php';
spl_autoload_register(function ($class) {
    $prefix = 'App\\';
    if (strpos($class, $prefix) === 0) {
        $file = __DIR__ . '/../app/' . str_replace('\\', '/', substr($class, strlen($prefix))) . '.php';
        if (is_file($file)) {
            require $file;
        }
    }
});

class FakeRoute
{
    private $methods;
    private $uri;
    private $uses;

    public function __construct($methods, $uri, $uses)
    {
        $this->methods = $methods;
        $this->uri = $uri;
        $this->uses = $uses;
    }

    public function methods() { return $this->methods; }
    public function uri() { return $this->uri; }
    public function getDomain() { return null; }
    public function getAction() { return array('uses' => $this->uses); }
    public function getActionName() { return $this->uses instanceof Closure ? 'Closure' : $this->uses; }
}

class FakeApp implements ArrayAccess
{
    public $routes = array();

    public function make($name) { return $this; }
    public function bootstrap() {}
    public function offsetExists($key): bool { return $key === 'router'; }
    public function offsetGet($key): mixed { return $this; }
    public function offsetSet($key, $value): void {}
    public function offsetUnset($key): void {}
    public function getRoutes() { return $this->routes; }
    public function call($callable) { return call_user_func($callable); }
}
