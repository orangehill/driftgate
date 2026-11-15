<?php
// Laravel's request classes, just enough for a form request's rules.
namespace Illuminate\Http {
    class Request
    {
        public static function create($uri, $method)
        {
            return new static();
        }
    }
}

namespace Illuminate\Foundation\Http {
    class FormRequest extends \Illuminate\Http\Request
    {
        public static function createFrom($from)
        {
            return new static();
        }

        public function setContainer($app)
        {
        }
    }
}
